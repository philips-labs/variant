package tva

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"

	"code.cloudfoundry.org/cfnetworking-cli-api/cfnetworking/cfnetv1"
	"code.cloudfoundry.org/cli/api/cloudcontroller/ccerror"
	"code.cloudfoundry.org/cli/api/cloudcontroller/ccv3"
	"code.cloudfoundry.org/cli/resources"
	clients "github.com/cloudfoundry-community/go-cf-clients-helper"
	"github.com/percona/promconfig"
	"gopkg.in/yaml.v2"
)

const (
	appMetadata   metadataType = "apps"
	exporterLabel              = "variant.tva/exporter"
	tenantLabel                = "variant.tva/tenant"
)

type Config struct {
	clients.Config
	PrometheusConfig string
	InternalDomainID string
	ThanosID         string
	ThanosURL        string
}

type Timeline struct {
	sync.Mutex
	*clients.Session
	targets       []promconfig.ScrapeConfig
	Selectors     []string
	defaultTenant bool
	startState    []cfnetv1.Policy
	startConfig   string
	config        Config
	reload        bool
	debug         bool
}

type App struct {
	resources.Application
	OrgName   string
	SpaceName string
}

func NewTimeline(config Config, opts ...OptionFunc) (*Timeline, error) {
	session, err := clients.NewSession(config.Config)
	if err != nil {
		return nil, fmt.Errorf("NewTimeline: %w", err)
	}
	timeline := &Timeline{
		Session:   session,
		Selectors: []string{fmt.Sprintf("%s=true", exporterLabel)},
		config:    config,
	}
	data, err := ioutil.ReadFile(config.PrometheusConfig)
	if err != nil {
		return nil, fmt.Errorf("read promethues config: %w", err)
	}
	var cfg promconfig.Config
	err = yaml.Unmarshal(data, &cfg)

	if err != nil {
		return nil, fmt.Errorf("load prometheus config: %w", err)
	}
	timeline.startConfig = string(data)
	timeline.startState = timeline.getCurrentPolicies()
	for _, o := range opts {
		if err := o(timeline); err != nil {
			return nil, err
		}
	}
	if timeline.debug {
		fmt.Printf("selectors:\n")
		for _, s := range timeline.Selectors {
			fmt.Printf("%s\n", s)
		}
	}
	return timeline, nil
}

func (t *Timeline) saveAndReload(newConfig string) error {
	if err := ioutil.WriteFile(t.config.PrometheusConfig, []byte(newConfig), 0644); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if !t.reload { // Prometheus/Thanos uses inotify
		return nil
	}
	resp, err := http.Post(t.config.ThanosURL+"/-/reload", "application/json", nil)
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}
	_, err = ioutil.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return err
}

// Reconcile calculates and applies network-polices and scrap configs
func (t *Timeline) Reconcile() error {
	t.Lock()
	defer t.Unlock()

	// Retrieve all relevant apps
	apps, _, err := t.V3().GetApplications(ccv3.Query{
		Key:    "label_selector",
		Values: t.Selectors,
	})
	// Retrieve default apps if applicable
	if len(t.Selectors) > 1 && t.defaultTenant {
		defaultApps, _, err := t.V3().GetApplications(ccv3.Query{
			Key: "label_selector",
			Values: []string{
				t.Selectors[0],
				fmt.Sprintf("!%s", tenantLabel)},
		})
		if err == nil {
			apps = append(apps, defaultApps...)
		}
	}

	if err != nil {
		return err
	}
	if t.debug {
		fmt.Printf("found %d matching selectors\n", len(apps))
	}
	// Determine the desired state
	var configs []promconfig.ScrapeConfig
	var generatedPolicies []cfnetv1.Policy
	for _, app := range apps {
		// Erase app from startTime if it show up on the timeline
		t.startState = prunePoliciesByDestination(t.startState, app.GUID)
		// Calculate policies and scrape_config sections for app
		policies, endpoints, _ := t.generatePoliciesAndScrapeConfigs(App{Application: app})
		generatedPolicies = append(generatedPolicies, policies...)
		configs = append(configs, endpoints...)
	}
	desiredState := uniqPolicies(append(t.startState, generatedPolicies...))
	currentState := t.getCurrentPolicies()
	if t.debug {
		fmt.Printf("desired: %d, current: %d\n", len(desiredState), len(currentState))
	}
	// Calculate add/prune
	var toAdd []cfnetv1.Policy
	for _, p := range desiredState {
		found := false
		for _, q := range currentState {
			if policyEqual(p, q) {
				found = true
			}
		}
		if !found {
			toAdd = append(toAdd, p)
		}
	}
	var toPrune []cfnetv1.Policy
	for _, p := range currentState {
		found := false
		for _, q := range desiredState {
			if policyEqual(p, q) {
				found = true
			}
		}
		if !found {
			toPrune = append(toPrune, p)
		}
	}

	// Do it
	fmt.Printf("adding: %d\n", len(toAdd))
	fmt.Printf("removing: %d\n", len(toPrune))
	if len(toPrune) > 0 {
		err := t.Networking().RemovePolicies(toPrune)
		if err != nil {
			fmt.Printf("error removing: %v\n", err)
		}
	}
	if len(toAdd) > 0 {
		err := t.Networking().CreatePolicies(toAdd)
		if err != nil {
			fmt.Printf("error creating: %v\n", err)
		}
	}
	t.targets = configs // Refresh the targets list

	// Generate new config
	var newCfg promconfig.Config

	err = yaml.Unmarshal([]byte(t.startConfig), &newCfg)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	for _, cfg := range configs {
		n := cfg
		newCfg.ScrapeConfigs = append(newCfg.ScrapeConfigs, &n)
	}
	output, err := yaml.Marshal(newCfg)
	if err != nil {
		return err
	}
	if t.debug {
		fmt.Printf("---config start---\n%s\n---config end---\n", string(output))
	}
	return t.saveAndReload(string(output))
}

func (t *Timeline) Targets() []promconfig.ScrapeConfig {
	t.Lock()
	targets := t.targets
	defer t.Unlock()
	return targets
}

func (t *Timeline) generatePoliciesAndScrapeConfigs(app App) ([]cfnetv1.Policy, []promconfig.ScrapeConfig, error) {
	var policies []cfnetv1.Policy
	var configs []promconfig.ScrapeConfig

	instanceCount := 0
	processes, _, err := t.V3().GetApplicationProcesses(app.GUID)
	for _, p := range processes {
		if p.Instances.IsSet && p.Instances.Value > instanceCount {
			instanceCount = p.Instances.Value
		}
	}
	if instanceCount == 0 {
		return policies, configs, fmt.Errorf("no instances found")
	}
	metadata, err := t.metadataRetrieve(appMetadata, app.GUID)
	if err != nil {
		return policies, configs, fmt.Errorf("metadataRetrieve: %w", err)
	}
	portNumber := 9090 // Default
	if port := metadata.Annotations["prometheus.exporter.port"]; port != nil {
		portNumber, err = strconv.Atoi(*port)
		if err != nil {
			return policies, configs, err
		}
	}
	scrapePath := "/metrics" // Default
	if path := metadata.Annotations["prometheus.exporter.path"]; path != nil {
		scrapePath = *path
	}
	jobName := app.Name // Default
	if name := metadata.Annotations["prometheus.exporter.job_name"]; name != nil {
		jobName = *name
	}
	scheme := "http" // Default
	if schema := metadata.Annotations["prometheus.exporter.scheme"]; schema != nil {
		scheme = *schema
	}
	policies = append(policies, t.newPolicy(app.GUID, portNumber))
	internalHost, err := t.internalHost(app)
	if err != nil {
		return policies, configs, err
	}
	var targets []string
	for count := 0; count < instanceCount; count++ {
		target := fmt.Sprintf("%d.%s:%d", count, internalHost, portNumber)
		targets = append(targets, target)
	}
	scrapeConfig := promconfig.ScrapeConfig{
		JobName:         jobName,
		HonorTimestamps: true,
		Scheme:          scheme,
		MetricsPath:     scrapePath,
		ServiceDiscoveryConfig: promconfig.ServiceDiscoveryConfig{
			StaticConfigs: []*promconfig.Group{
				{
					Targets: targets,
					Labels: map[string]string{
						"cf_app_name": app.Name,
					},
				},
			},
		},
	}
	instanceName := ""
	if name := metadata.Annotations["prometheus.exporter.instance_name"]; name != nil {
		instanceName = *name
	}
	if instanceName != "" {
		targetRegex := "([^.]*).(.*)" // This match our own target format: ${1} = instanceIndex, ${2} = host:port
		if regex := metadata.Annotations["prometheus.exporter.target_regex"]; regex != nil {
			targetRegex = *regex
		}
		scrapeConfig.RelabelConfigs = append(scrapeConfig.RelabelConfigs, &promconfig.RelabelConfig{
			TargetLabel:  "instance",
			SourceLabels: []string{"__param_target"},
			Replacement:  instanceName,
			Regex:        targetRegex,
		})
	}
	if port := metadata.Annotations["prometheus.targets.port"]; port != nil {
		targetsPort, err := strconv.Atoi(*port)
		if err != nil {
			return policies, configs, err
		}
		targetsPath := "/targets"
		if path := metadata.Annotations["prometheus.targets.path"]; path != nil {
			targetsPath = *path
		}
		targetsURL := fmt.Sprintf("http://%s:%d%s", internalHost, targetsPort, targetsPath)
		policies = append(policies, t.newPolicy(app.GUID, targetsPort))
		scrapeConfig.RelabelConfigs = append(scrapeConfig.RelabelConfigs,
			&promconfig.RelabelConfig{
				SourceLabels: []string{"__address__"},
				TargetLabel:  "__param_target",
			},
			&promconfig.RelabelConfig{
				SourceLabels: []string{"__param_target"},
				TargetLabel:  "instance",
			})
		scrapeConfig.ServiceDiscoveryConfig = promconfig.ServiceDiscoveryConfig{
			HTTPSDConfigs: []*promconfig.HTTPSDConfig{
				{URL: targetsURL},
			},
		}
	}
	configs = append(configs, scrapeConfig)
	return policies, configs, nil
}

func (t *Timeline) getCurrentPolicies() []cfnetv1.Policy {
	allPolicies, _ := t.Networking().ListPolicies(t.config.ThanosID)
	var policies []cfnetv1.Policy
	for _, p := range allPolicies {
		if p.Source.ID == t.config.ThanosID {
			policies = append(policies, p)
		}
	}
	return policies
}

func (t *Timeline) newPolicy(destination string, port int) cfnetv1.Policy {
	return cfnetv1.Policy{
		Source: cfnetv1.PolicySource{ID: t.config.ThanosID},
		Destination: cfnetv1.PolicyDestination{
			ID:       destination,
			Protocol: cfnetv1.PolicyProtocolTCP,
			Ports:    cfnetv1.Ports{Start: port, End: port},
		},
	}
}

func pathMetadata(m metadataType, guid string) string {
	return fmt.Sprintf("/v3/%s/%s", m, guid)
}

func (t *Timeline) internalHost(app App) (string, error) {
	client := t.Session.V2()
	routes, _, err := client.GetApplicationRoutes(app.GUID)
	if err != nil {
		return "", err
	}
	for _, r := range routes {
		if r.DomainGUID == t.config.InternalDomainID {
			return fmt.Sprintf("%s.%s", r.Host, "apps.internal"), nil
		}
	}
	return "", fmt.Errorf("no apps.internal route found")
}

func (t *Timeline) metadataRetrieve(m metadataType, guid string) (Metadata, error) {
	client := t.Session.Raw()
	req, err := client.NewRequest("GET", pathMetadata(m, guid), nil)
	if err != nil {
		return Metadata{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return Metadata{}, err
	}

	defer func() {
		err := resp.Body.Close()
		if err != nil {
			panic(err)
		}
	}()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return Metadata{}, err
	}

	if resp.StatusCode != 200 {
		if resp.StatusCode == 404 {
			return Metadata{}, nil
		}
		return Metadata{}, ccerror.RawHTTPStatusError{
			StatusCode:  resp.StatusCode,
			RawResponse: b,
		}
	}

	var metadataReq MetadataRequest
	err = json.Unmarshal(b, &metadataReq)
	if err != nil {
		return Metadata{}, err
	}
	return metadataReq.Metadata, nil
}

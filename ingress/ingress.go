package ingress

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/config"
	"github.com/cloudflare/cloudflared/logger"
)

var (
	ErrNoIngressRules             = errors.New("No ingress rules were specified in the config file")
	errLastRuleNotCatchAll        = errors.New("The last ingress rule must match all hostnames (i.e. it must be missing, or must be \"*\")")
	errBadWildcard                = errors.New("Hostname patterns can have at most one wildcard character (\"*\") and it can only be used for subdomains, e.g. \"*.example.com\"")
	ErrURLIncompatibleWithIngress = errors.New("You can't set the --url flag (or $TUNNEL_URL) when using multiple-origin ingress rules")
)

// FindMatchingRule returns the index of the Ingress Rule which matches the given
// hostname and path. This function assumes the last rule matches everything,
// which is the case if the rules were instantiated via the ingress#Validate method
func (ing Ingress) FindMatchingRule(hostname, path string) (*Rule, int) {
	for i, rule := range ing.Rules {
		if rule.Matches(hostname, path) {
			return &rule, i
		}
	}
	i := len(ing.Rules) - 1
	return &ing.Rules[i], i
}

func matchHost(ruleHost, reqHost string) bool {
	if ruleHost == reqHost {
		return true
	}

	// Validate hostnames that use wildcards at the start
	if strings.HasPrefix(ruleHost, "*.") {
		toMatch := strings.TrimPrefix(ruleHost, "*.")
		return strings.HasSuffix(reqHost, toMatch)
	}
	return false
}

// Ingress maps eyeball requests to origins.
type Ingress struct {
	Rules    []Rule
	defaults OriginRequestConfig
}

// NewSingleOrigin constructs an Ingress set with only one rule, constructed from
// legacy CLI parameters like --url or --no-chunked-encoding.
func NewSingleOrigin(c *cli.Context, compatibilityMode bool, logger logger.Service) (Ingress, error) {

	service, err := parseSingleOriginService(c, compatibilityMode)
	if err != nil {
		return Ingress{}, err
	}

	// Construct an Ingress with the single rule.
	ing := Ingress{
		Rules: []Rule{
			{
				Service: service,
			},
		},
		defaults: originRequestFromSingeRule(c),
	}
	return ing, err
}

// Get a single origin service from the CLI/config.
func parseSingleOriginService(c *cli.Context, compatibilityMode bool) (OriginService, error) {
	if c.IsSet("hello-world") {
		return new(helloWorld), nil
	}
	if c.IsSet("url") {
		originURLStr, err := config.ValidateUrl(c, compatibilityMode)
		if err != nil {
			return nil, errors.Wrap(err, "Error validating origin URL")
		}
		originURL, err := url.Parse(originURLStr)
		if err != nil {
			return nil, errors.Wrap(err, "couldn't parse origin URL")
		}
		return &localService{URL: originURL, RootURL: originURL}, nil
	}
	if c.IsSet("unix-socket") {
		path, err := config.ValidateUnixSocket(c)
		if err != nil {
			return nil, errors.Wrap(err, "Error validating --unix-socket")
		}
		return &unixSocketPath{path: path}, nil
	}
	return nil, errors.New("You must either set ingress rules in your config file, or use --url or use --unix-socket")
}

// IsEmpty checks if there are any ingress rules.
func (ing Ingress) IsEmpty() bool {
	return len(ing.Rules) == 0
}

// StartOrigins will start any origin services managed by cloudflared, e.g. proxy servers or Hello World.
func (ing Ingress) StartOrigins(wg *sync.WaitGroup, log logger.Service, shutdownC <-chan struct{}, errC chan error) error {
	for _, rule := range ing.Rules {
		if err := rule.Service.start(wg, log, shutdownC, errC, rule.Config); err != nil {
			return err
		}
	}
	return nil
}

// CatchAll returns the catch-all rule (i.e. the last rule)
func (ing Ingress) CatchAll() *Rule {
	return &ing.Rules[len(ing.Rules)-1]
}

func validate(ingress []config.UnvalidatedIngressRule, defaults OriginRequestConfig) (Ingress, error) {
	rules := make([]Rule, len(ingress))
	for i, r := range ingress {
		var service OriginService

		if prefix := "unix:"; strings.HasPrefix(r.Service, prefix) {
			// No validation necessary for unix socket filepath services
			path := strings.TrimPrefix(r.Service, prefix)
			service = &unixSocketPath{path: path}
		} else if prefix := "http_status:"; strings.HasPrefix(r.Service, prefix) {
			status, err := strconv.Atoi(strings.TrimPrefix(r.Service, prefix))
			if err != nil {
				return Ingress{}, errors.Wrap(err, "invalid HTTP status")
			}
			srv := newStatusCode(status)
			service = &srv
		} else if r.Service == "hello_world" || r.Service == "hello-world" || r.Service == "helloworld" {
			service = new(helloWorld)
		} else {
			// Validate URL services
			u, err := url.Parse(r.Service)
			if err != nil {
				return Ingress{}, err
			}

			if u.Scheme == "" || u.Hostname() == "" {
				return Ingress{}, fmt.Errorf("The service %s must have a scheme and a hostname", r.Service)
			}

			if u.Path != "" {
				return Ingress{}, fmt.Errorf("%s is an invalid address, ingress rules don't support proxying to a different path on the origin service. The path will be the same as the eyeball request's path", r.Service)
			}
			serviceURL := localService{URL: u}
			service = &serviceURL
		}

		// Ensure that there are no wildcards anywhere except the first character
		// of the hostname.
		if strings.LastIndex(r.Hostname, "*") > 0 {
			return Ingress{}, errBadWildcard
		}

		// The last rule should catch all hostnames.
		isCatchAllRule := (r.Hostname == "" || r.Hostname == "*") && r.Path == ""
		isLastRule := i == len(ingress)-1
		if isLastRule && !isCatchAllRule {
			return Ingress{}, errLastRuleNotCatchAll
		}
		// ONLY the last rule should catch all hostnames.
		if !isLastRule && isCatchAllRule {
			return Ingress{}, errRuleShouldNotBeCatchAll{i: i, hostname: r.Hostname}
		}

		var pathRegex *regexp.Regexp
		if r.Path != "" {
			var err error
			pathRegex, err = regexp.Compile(r.Path)
			if err != nil {
				return Ingress{}, errors.Wrapf(err, "Rule #%d has an invalid regex", i+1)
			}
		}

		rules[i] = Rule{
			Hostname: r.Hostname,
			Service:  service,
			Path:     pathRegex,
			Config:   setConfig(defaults, r.OriginRequest),
		}
	}
	return Ingress{Rules: rules, defaults: defaults}, nil
}

type errRuleShouldNotBeCatchAll struct {
	i        int
	hostname string
}

func (e errRuleShouldNotBeCatchAll) Error() string {
	return fmt.Sprintf("Rule #%d is matching the hostname '%s', but "+
		"this will match every hostname, meaning the rules which follow it "+
		"will never be triggered.", e.i+1, e.hostname)
}

// ParseIngress parses ingress rules, but does not send HTTP requests to the origins.
func ParseIngress(conf *config.Configuration) (Ingress, error) {
	if len(conf.Ingress) == 0 {
		return Ingress{}, ErrNoIngressRules
	}
	return validate(conf.Ingress, originRequestFromYAML(conf.OriginRequest))
}

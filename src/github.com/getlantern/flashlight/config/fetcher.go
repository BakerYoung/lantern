package config

import (
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/getlantern/errlog"
	"github.com/getlantern/flashlight/util"
	"github.com/getlantern/yamlconf"

	"code.google.com/p/go-uuid/uuid"
)

const (
	etag                  = "X-Lantern-Etag"
	ifNoneMatch           = "X-Lantern-If-None-Match"
	userIDHeader          = "X-Lantern-User-Id"
	tokenHeader           = "X-Lantern-Pro-Token"
	chainedCloudConfigURL = "http://config.getiantem.org/cloud.yaml.gz"

	// This is over HTTP because proxies do not forward X-Forwarded-For with HTTPS
	// and because we only support falling back to direct domain fronting through
	// the local proxy for HTTP.
	frontedCloudConfigURL = "http://d2wi0vwulmtn99.cloudfront.net/cloud.yaml.gz"
)

var (
	// CloudConfigPollInterval is the period to wait befween checks for new
	// global configuration settings.
	CloudConfigPollInterval = 1 * time.Minute
)

// fetcher periodically fetches the latest cloud configuration.
type fetcher struct {
	lastCloudConfigETag map[string]string
	user                UserConfig
	httpFetcher         util.HTTPFetcher
}

// UserConfig retrieves any custom user info for fetching the config.
type UserConfig interface {
	GetUserID() string
	GetToken() string
}

// NewFetcher creates a new configuration fetcher with the specified
// interface for obtaining the user ID and token if those are populated.
func NewFetcher(conf UserConfig, httpFetcher util.HTTPFetcher) Fetcher {
	return &fetcher{lastCloudConfigETag: map[string]string{}, user: conf, httpFetcher: httpFetcher}
}

func (cf *fetcher) pollForConfig(currentCfg yamlconf.Config, stickyConfig bool) (mutate func(yamlconf.Config) error, waitTime time.Duration, err error) {
	log.Debugf("Polling for config")
	// By default, do nothing
	mutate = func(ycfg yamlconf.Config) error {
		// do nothing
		return nil
	}
	cfg := currentCfg.(*Config)
	waitTime = cf.cloudPollSleepTime()
	if cfg.CloudConfig == "" {
		log.Debugf("No cloud config URL!")
		// Config doesn't have a CloudConfig, just ignore
		return mutate, waitTime, nil
	}
	if stickyConfig {
		log.Debugf("Not downloading remote config with sticky config flag set")
		return mutate, waitTime, nil
	}

	if bytes, err := cf.fetchCloudConfig(cfg); err != nil {
		elog.Log(err, errlog.WithOp("fetch-cloud-config"))
		return mutate, waitTime, err
	} else if bytes != nil {
		// bytes will be nil if the config is unchanged (not modified)
		mutate = func(ycfg yamlconf.Config) error {
			log.Debugf("Merging cloud configuration")
			cfg := ycfg.(*Config)

			err := cfg.updateFrom(bytes)
			if cfg.Client.ChainedServers != nil {
				log.Debugf("Adding %d chained servers", len(cfg.Client.ChainedServers))
				for _, s := range cfg.Client.ChainedServers {
					log.Debugf("Got chained server: %v", s.Addr)
				}
			}
			return err
		}
	} else {
		log.Debugf("Bytes are nil - config not modified.")
	}
	return mutate, waitTime, nil
}

func (cf *fetcher) fetchCloudConfig(cfg *Config) ([]byte, error) {
	log.Debugf("Fetching cloud config from %v (%v)", cfg.CloudConfig, cfg.FrontedCloudConfig)

	url := cfg.CloudConfig
	cb := "?" + uuid.New()
	nocache := url + cb
	req, err := http.NewRequest("GET", nocache, nil)
	if err != nil {
		return nil, fmt.Errorf("Unable to construct request for cloud config at %s: %s", nocache, err)
	}
	if cf.lastCloudConfigETag[url] != "" {
		// Don't bother fetching if unchanged
		req.Header.Set(ifNoneMatch, cf.lastCloudConfigETag[url])
	}

	req.Header.Set("Accept", "application/x-gzip")
	// Prevents intermediate nodes (domain-fronters) from caching the content
	req.Header.Set("Cache-Control", "no-cache")
	// Set the fronted URL to lookup the config in parallel using chained and domain fronted servers.
	req.Header.Set("Lantern-Fronted-URL", cfg.FrontedCloudConfig+cb)

	id := cf.user.GetUserID()
	if id != "" {
		req.Header.Set(userIDHeader, id)
	}
	tok := cf.user.GetToken()
	if tok != "" {
		req.Header.Set(tokenHeader, tok)
	}

	// make sure to close the connection after reading the Body
	// this prevents the occasional EOFs errors we're seeing with
	// successive requests
	req.Close = true

	resp, err := cf.httpFetcher.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch cloud config at %s: %s", url, err)
	}
	dump, dumperr := httputil.DumpResponse(resp, false)
	if dumperr != nil {
		elog.Log(dumperr, errlog.WithOp("dump-response"))
	} else {
		log.Debugf("Response headers: \n%v", string(dump))
	}
	defer func() {
		if closeerr := resp.Body.Close(); closeerr != nil {
			log.Debugf("Error closing response body: %v", closeerr)
		}
	}()

	if resp.StatusCode == 304 {
		log.Debugf("Config unchanged in cloud")
		return nil, nil
	} else if resp.StatusCode != 200 {
		if dumperr != nil {
			return nil, fmt.Errorf("Bad config response code: %v", resp.StatusCode)
		}
		return nil, fmt.Errorf("Bad config resp:\n%v", string(dump))
	}

	cf.lastCloudConfigETag[url] = resp.Header.Get(etag)
	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Unable to open gzip reader: %s", err)
	}
	log.Debugf("Fetched cloud config")
	return ioutil.ReadAll(gzReader)
}

// cloudPollSleepTime adds some randomization to our requests to make them
// less distinguishing on the network.
func (cf *fetcher) cloudPollSleepTime() time.Duration {
	return time.Duration((CloudConfigPollInterval.Nanoseconds() / 2) + rand.Int63n(CloudConfigPollInterval.Nanoseconds()))
}

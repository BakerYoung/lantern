package analytics

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/getlantern/eventual"
	"github.com/getlantern/flashlight/client"
	"github.com/getlantern/flashlight/geolookup"
	"github.com/getlantern/flashlight/logging"
	"github.com/getlantern/flashlight/util"
	"github.com/kardianos/osext"

	"github.com/getlantern/errlog"
	"github.com/getlantern/golog"
)

const (
	trackingID = "UA-21815217-12"

	// endpoint is the endpoint to report GA data to.
	endpoint = `https://ssl.google-analytics.com/collect`
)

var (
	log  = golog.LoggerFor("flashlight.analytics")
	elog = errlog.ErrorLoggerFor("flashlight.analytics")

	maxWaitForIP = math.MaxInt32 * time.Second

	// We get the user agent to use from live data on the proxy, but don't wait
	// for it forever!
	maxWaitForUserAgent = 30 * time.Second

	// This allows us to report a real user agent from clients we see on the
	// proxy.
	userAgent = eventual.NewValue()

	// Hash of the executable
	hash = getExecutableHash()
)

// Start starts the GA session with the given data.
func Start(deviceID, version string) func() {
	return start(deviceID, version, geolookup.GetIP, maxWaitForUserAgent, trackSession)
}

// start starts the GA session with the given data.
func start(deviceID, version string, ipFunc func(time.Duration) string, uaWait time.Duration,
	transport func(string, eventual.Getter)) func() {
	var addr atomic.Value
	go func() {
		logging.AddUserAgentListener(func(agent string) {
			userAgent.Set(agent)
		})
		ip := ipFunc(maxWaitForIP)
		if ip == "" {
			elog.Log(fmt.Errorf("No IP found"),
				errlog.WithOp("geolookup"),
				errlog.WithField("waitSeconds", strconv.FormatInt(int64(maxWaitForIP/time.Second), 10)),
			)
			return
		}
		addr.Store(ip)
		log.Debugf("Starting analytics session with ip %v", ip)
		startSession(ip, version, client.Addr, deviceID, transport, uaWait)
	}()

	stop := func() {
		if addr.Load() != nil {
			ip := addr.Load().(string)
			log.Debugf("Ending analytics session with ip %v", ip)
			endSession(ip, version, client.Addr, deviceID, transport, uaWait)
		}
	}
	return stop
}

func sessionVals(ip, version, clientID, sc string, uaWait time.Duration) string {
	vals := make(url.Values, 0)

	vals.Add("v", "1")
	vals.Add("cid", clientID)
	vals.Add("tid", trackingID)

	if ip != "" {
		// Override the users IP so we get accurate geo data.
		vals.Add("uip", ip)
	}

	// Make call to anonymize the user's IP address -- basically a policy thing where
	// Google agrees not to store it.
	vals.Add("aip", "1")

	vals.Add("dp", "localhost")
	vals.Add("t", "pageview")

	// Custom dimension for the Lantern version
	vals.Add("cd1", version)

	// Custom dimension for the hash of the executable
	vals.Add("cd2", hash)

	// This sets the user agent to a real user agent the user is using. We
	// wait 30 seconds for some traffic to come through.
	ua, found := userAgent.Get(uaWait)
	if found {
		vals.Add("ua", ua.(string))
	}

	// This forces the recording of the session duration. It must be either
	// "start" or "end". See:
	// https://developers.google.com/analytics/devguides/collection/protocol/v1/parameters
	vals.Add("sc", sc)

	// Make this a non-interaction hit that bypasses things like bounce rate. See:
	// https://developers.google.com/analytics/devguides/collection/protocol/v1/parameters#ni
	vals.Add("ni", "1")
	return vals.Encode()
}

// GetExecutableHash returns the hash of the currently running executable.
// If there's an error getting the hash, this returns
func getExecutableHash() string {
	// We don't know how to get a useful hash here for Android but also this
	// code isn't currently called on Android, so just guard against Something
	// bad happening here.
	if runtime.GOOS == "android" {
		return "android"
	}
	if lanternPath, err := osext.Executable(); err != nil {
		log.Debugf("Could not get path to executable %v", err)
		return err.Error()
	} else {
		if b, er := util.GetFileHash(lanternPath); er != nil {
			return er.Error()
		} else {
			return b
		}
	}
}

func endSession(ip string, version string, proxyAddrFN eventual.Getter,
	clientID string, transport func(string, eventual.Getter), uaWait time.Duration) {
	args := sessionVals(ip, version, clientID, "end", uaWait)
	transport(args, proxyAddrFN)
}

func startSession(ip string, version string, proxyAddrFN eventual.Getter,
	clientID string, transport func(string, eventual.Getter), uaWait time.Duration) {
	args := sessionVals(ip, version, clientID, "start", uaWait)
	transport(args, proxyAddrFN)
}

func trackSession(args string, proxyAddrFN eventual.Getter) {
	r, err := http.NewRequest("POST", endpoint, bytes.NewBufferString(args))

	if err != nil {
		elog.Log(err, errlog.WithOp("new-ga-request"))
		return
	}

	r.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Add("Content-Length", strconv.Itoa(len(args)))

	if req, er := httputil.DumpRequestOut(r, true); er != nil {
		log.Debugf("Could not dump request: %v", er)
	} else {
		log.Debugf("Full analytics request: %v", string(req))
	}

	var httpClient *http.Client
	httpClient, err = util.HTTPClient("", proxyAddrFN)
	if err != nil {
		elog.Log(err, errlog.WithOp("create-http-client"))
		return
	}
	resp, err := httpClient.Do(r)
	if err != nil {
		elog.Log(err, errlog.WithOp("send-http-request"))
		return
	}
	log.Debugf("Successfully sent request to GA: %s", resp.Status)
	if err := resp.Body.Close(); err != nil {
		log.Debugf("Unable to close response body: %v", err)
	}
}

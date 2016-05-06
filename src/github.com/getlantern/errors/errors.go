/*
Package errors defines error interfaces and types used across Lantern project
and implements functions to manipulate them.

Guildlines to report error:

1. Report at the end of error propagation chain, that is, before the code
resumes from the error or makes a decision based on it. The purpose is to avoid
reporting repetitively, and prevent lower level of code from depending on this
package.
*/
package errors

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/getlantern/golog"
	"github.com/getlantern/jibber_jabber"
	"github.com/getlantern/osversion"
)

// Op is the operation caused an error, modeled after net.OpError.Op
type Op string

const (
	OpDial  Op = "dial"
	OpRead  Op = "read"
	OpWrite Op = "write"
	OpClose Op = "close"
)

type systemInfo struct {
	OSType    string `json:"osType"`
	OSVersion string `json:"osVersion"`
	OSArch    string `json:"osArch"`
}

type UserLocale struct {
	TimeZone string `json:"timeZone,omitempty"`
	Language string `json:"language,omitempty"`
	Country  string `json:"country,omitempty"`
}

// ProxyType is the type of various proxy channel
type ProxyType string

const (
	// direct access, no proxying at all
	NoProxy ProxyType = "no"
	// access through Lantern hosted chained server
	ChainedProxy ProxyType = "chained"
	// access through domain fronting
	FrontedProxy ProxyType = "fronted"
	// access through direct domain fronting
	DirectFrontedProxy ProxyType = "DDF"
)

// ProxyingInfo encapsulates fields to describe an access through a proxy channel.
type ProxyingInfo struct {
	ProxyType  ProxyType `json:"proxyType,omitempty"`
	LocalAddr  string    `json:"localAddr,omitempty"`
	ProxyAddr  string    `json:"proxyAddr,omitempty"`
	ProxyDC    string    `json:"proxyDataCenter,omitempty"`
	OriginSite string    `json:"originSite,omitempty"`
	Scheme     string    `json:"scheme,omitempty"`
}

// UserAgentInfo encapsulates traits of the browsers or 3rd party applications
// directing traffic through Lantern.
type UserAgentInfo struct {
	UserAgent string `json:"userAgent,omitempty"`
}

// Error wraps system and application errors in unified structure
type Error struct {
	// Go package reports the error
	GoPackage string `json:"package"`
	// Go type name or constant/variable name of the error
	GoType string `json:"type"`
	// Error description, by either Go library or application
	Desc string `json:"desc"`
	// The operation which triggers the error to happen
	Op Op `json:"operation,omitempty"`
	// Any extra fields
	Extra map[string]string `json:"extra,omitempty"`
	*systemInfo
	*ProxyingInfo
	*UserLocale
	*UserAgentInfo
}

// Customized marshaller to marshal extra fields to same level as other struct fields
/*func (e Error) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	// safe to ignore return value as error returned is always nil
	_, _ = buf.WriteString(fmt.Sprintf(`{"package":"%s","type":"%s","desc":"%s"`, e.GoPackage, e.GoType, e.Desc))
	if e.Extra != nil && len(e.Extra) > 0 {
		_, _ = buf.WriteString(",")
		for k, v := range e.Extra {
			_, _ = buf.WriteString(fmt.Sprintf(`"%s":"%s"`, k, v))
		}
	}
	_, _ = buf.WriteString("}")
	return buf.Bytes(), nil
}*/

type ErrorCollector struct {
	goPackage string
	logger    golog.Logger
	*systemInfo
}

type withFunc func(e *Error)

func WithOp(op Op) withFunc {
	return func(e *Error) {
		e.Op = op
	}
}

func WithProxy(info *ProxyingInfo) withFunc {
	return func(e *Error) {
		e.ProxyingInfo = info
	}
}

func WithLocale() withFunc {
	return func(e *Error) {
		lang, _ := jibber_jabber.DetectLanguage()
		country, _ := jibber_jabber.DetectTerritory()
		e.UserLocale = &UserLocale{
			time.Now().Format("MST"),
			lang,
			country,
		}
	}
}

func WithUserAgent(info *UserAgentInfo) withFunc {
	return func(e *Error) {
		e.UserAgentInfo = info
	}
}

func (c *ErrorCollector) Log(err error, with ...withFunc) {
	errOp, goType, desc, extra := parseError(err)
	actual := &Error{
		GoPackage:  c.goPackage,
		GoType:     goType,
		Desc:       desc,
		Op:         Op(errOp),
		Extra:      extra,
		systemInfo: c.systemInfo,
	}
	for _, f := range with {
		f(actual)
	}
	currentReporter.Report(actual)
}

func NewErrorCollector(goPackage string) *ErrorCollector {
	version, _ := osversion.GetHumanReadable()
	return &ErrorCollector{
		goPackage: goPackage,
		logger:    golog.LoggerFor(goPackage),
		systemInfo: &systemInfo{
			OSType:    runtime.GOOS,
			OSArch:    runtime.GOARCH,
			OSVersion: version,
		},
	}
}

type Reporter interface {
	Report(*Error)
}

var currentReporter Reporter = &StdReporter{}

func ReportTo(r Reporter) {
	currentReporter = r
}

func toJSON(e *Error) []byte {
	b, err := json.Marshal(e)
	if err != nil {
		panic(fmt.Sprintf("failed to convert error to json: %+v", err))
	}
	return b
}

type StdReporter struct {
}

func (l StdReporter) Report(e *Error) {
	fmt.Printf("%+v", string(toJSON(e)))
}

func parseError(err error) (op string, goType string, desc string, extra map[string]string) {
	extra = make(map[string]string)

	// interfaces
	if _, ok := err.(net.Error); ok {
		if opError, ok := err.(*net.OpError); ok {
			op = opError.Op
			if opError.Source != nil {
				extra["localAddr"] = opError.Source.String()
			}
			if opError.Addr != nil {
				extra["remoteAddr"] = opError.Addr.String()
			}
			extra["network"] = opError.Net
			err = opError.Err
		}
		switch actual := err.(type) {
		case *net.AddrError:
			goType = "net.AddrError"
			desc = actual.Err
			extra["addr"] = actual.Addr
		case *net.DNSError:
			goType = "net.DNSError"
			desc = actual.Err
			extra["domain"] = actual.Name
			if actual.Server != "" {
				extra["dnsServer"] = actual.Server
			}
		case *net.InvalidAddrError:
			goType = "net.InvalidAddrError"
			desc = actual.Error()
		case *net.ParseError:
			goType = "net.ParseError"
			desc = "invalid " + actual.Type
			extra["textToParse"] = actual.Text
		case net.UnknownNetworkError:
			goType = "net.UnknownNetworkError"
			desc = "unknown network"
		case syscall.Errno:
			goType = "syscall.Errno"
			desc = actual.Error()
		case *url.Error:
			goType = "url.Error"
			desc = actual.Err.Error()
			op = actual.Op
		default:
			goType = reflect.TypeOf(err).String()
			desc = err.Error()
		}
		return
	}
	if _, ok := err.(runtime.Error); ok {
		desc = err.Error()
		switch err.(type) {
		case *runtime.TypeAssertionError:
			goType = "runtime.TypeAssertionError"
		default:
			goType = reflect.TypeOf(err).String()
		}
		return
	}

	// structs
	switch actual := err.(type) {
	case *http.ProtocolError:
		desc = actual.ErrorString
		if name, ok := httpProtocolErrors[err]; ok {
			goType = name
		} else {
			goType = "http.ProtocolError"
		}
	case url.EscapeError, *url.EscapeError:
		goType = "url.EscapeError"
		desc = "invalid URL escape"
	case url.InvalidHostError, *url.InvalidHostError:
		goType = "url.InvalidHostError"
		desc = "invalid character in host name"
	case *textproto.Error:
		goType = "textproto.Error"
		desc = actual.Error()
	case textproto.ProtocolError, *textproto.ProtocolError:
		goType = "textproto.ProtocolError"
		desc = actual.Error()

	case tls.RecordHeaderError:
		goType = "tls.RecordHeaderError"
		desc = actual.Msg
		extra["header"] = hex.EncodeToString(actual.RecordHeader[:])
	case x509.CertificateInvalidError:
		goType = "x509.CertificateInvalidError"
		desc = actual.Error()
	case x509.ConstraintViolationError:
		goType = "x509.ConstraintViolationError"
		desc = actual.Error()
	case x509.HostnameError:
		goType = "x509.HostnameError"
		desc = actual.Error()
		extra["host"] = actual.Host
	case x509.InsecureAlgorithmError:
		goType = "x509.InsecureAlgorithmError"
		desc = actual.Error()
	case x509.SystemRootsError:
		goType = "x509.SystemRootsError"
		desc = actual.Error()
	case x509.UnhandledCriticalExtension:
		goType = "x509.UnhandledCriticalExtension"
		desc = actual.Error()
	case x509.UnknownAuthorityError:
		goType = "x509.UnknownAuthorityError"
		desc = actual.Error()
	case hex.InvalidByteError:
		goType = "hex.InvalidByteError"
		desc = "invalid byte"
	case *json.InvalidUTF8Error:
		goType = "json.InvalidUTF8Error"
		desc = "invalid UTF-8 in string"
	case *json.InvalidUnmarshalError:
		goType = "json.InvalidUnmarshalError"
		desc = actual.Error()
	case *json.MarshalerError:
		goType = "json.MarshalerError"
		desc = actual.Error()
	case *json.SyntaxError:
		goType = "json.SyntaxError"
		desc = actual.Error()
	case *json.UnmarshalFieldError:
		goType = "json.UnmarshalFieldError"
		desc = actual.Error()
	case *json.UnmarshalTypeError:
		goType = "json.UnmarshalTypeError"
		desc = actual.Error()
	case *json.UnsupportedTypeError:
		goType = "json.UnsupportedTypeError"
		desc = actual.Error()
	case *json.UnsupportedValueError:
		goType = "json.UnsupportedValueError"
		desc = actual.Error()

	case *os.LinkError:
		goType = "textproto.ProtocolError"
		desc = actual.Error()
	case *os.PathError:
		goType = "os.PathError"
		op = actual.Op
		desc = actual.Err.Error()
	case *os.SyscallError:
		goType = "os.SyscallError"
		op = actual.Syscall
		desc = actual.Err.Error()
	case *exec.Error:
		goType = "exec.Error"
		desc = actual.Err.Error()
	case *exec.ExitError:
		goType = "exec.ExitError"
		desc = actual.Error()
		// TODO: limit the length
		extra["stderr"] = string(actual.Stderr)
	case *strconv.NumError:
		goType = "strconv.NumError"
		desc = actual.Err.Error()
		extra["function"] = actual.Func
	case *time.ParseError:
		goType = "time.ParseError"
		desc = actual.Message
	default:
		desc = err.Error()
		if t, ok := miscErrors[err]; ok {
			goType = t
		} else {
			goType = reflect.TypeOf(err).String()
		}
		return
	}
	return
}

var httpProtocolErrors = map[error]string{
	http.ErrHeaderTooLong:        "http.ErrHeaderTooLong",
	http.ErrShortBody:            "http.ErrShortBody",
	http.ErrNotSupported:         "http.ErrNotSupported",
	http.ErrUnexpectedTrailer:    "http.ErrUnexpectedTrailer",
	http.ErrMissingContentLength: "http.ErrMissingContentLength",
	http.ErrNotMultipart:         "http.ErrNotMultipart",
	http.ErrMissingBoundary:      "http.ErrMissingBoundary",
}

var miscErrors = map[error]string{
	bufio.ErrInvalidUnreadByte: "bufio.ErrInvalidUnreadByte",
	bufio.ErrInvalidUnreadRune: "bufio.ErrInvalidUnreadRune",
	bufio.ErrBufferFull:        "bufio.ErrBufferFull",
	bufio.ErrNegativeCount:     "bufio.ErrNegativeCount",
	bufio.ErrTooLong:           "bufio.ErrTooLong",
	bufio.ErrNegativeAdvance:   "bufio.ErrNegativeAdvance",
	bufio.ErrAdvanceTooFar:     "bufio.ErrAdvanceTooFar",
	bufio.ErrFinalToken:        "bufio.ErrFinalToken",

	http.ErrWriteAfterFlush:    "http.ErrWriteAfterFlush",
	http.ErrBodyNotAllowed:     "http.ErrBodyNotAllowed",
	http.ErrHijacked:           "http.ErrHijacked",
	http.ErrContentLength:      "http.ErrContentLength",
	http.ErrBodyReadAfterClose: "http.ErrBodyReadAfterClose",
	http.ErrHandlerTimeout:     "http.ErrHandlerTimeout",
	http.ErrLineTooLong:        "http.ErrLineTooLong",
	http.ErrMissingFile:        "http.ErrMissingFile",
	http.ErrNoCookie:           "http.ErrNoCookie",
	http.ErrNoLocation:         "http.ErrNoLocation",
	http.ErrSkipAltProtocol:    "http.ErrSkipAltProtocol",

	io.EOF:              "io.EOF",
	io.ErrClosedPipe:    "io.ErrClosedPipe",
	io.ErrNoProgress:    "io.ErrNoProgress",
	io.ErrShortBuffer:   "io.ErrShortBuffer",
	io.ErrShortWrite:    "io.ErrShortWrite",
	io.ErrUnexpectedEOF: "io.ErrUnexpectedEOF",

	os.ErrInvalid:    "os.ErrInvalid",
	os.ErrPermission: "os.ErrPermission",
	os.ErrExist:      "os.ErrExist",
	os.ErrNotExist:   "os.ErrNotExist",

	exec.ErrNotFound: "exec.ErrNotFound",

	x509.ErrUnsupportedAlgorithm: "x509.ErrUnsupportedAlgorithm",
	x509.IncorrectPasswordError:  "x509.IncorrectPasswordError",

	hex.ErrLength: "hex.ErrLength",
}

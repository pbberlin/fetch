// Package fetches http resources,
// for appengine and standalone go programmes.
package fetch

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/context"

	"github.com/zew/util"

	"google.golang.org/appengine"
	"google.golang.org/appengine/urlfetch"
)

var MsgNoRedirects = "redirect cancelled"

type Job struct {
	URL                 string
	Req                 *http.Request // holds the final request Url for inspection
	Timeout             time.Duration
	OnRedirect          int // 1 => call off upon redirects
	LogLevel            int
	ForceProtocol       string
	ForceHttps          bool          // Force https even on dev server; forgot why we would need this
	AeReq               *http.Request // Appengine Request - only for getting an AE context
	the_response_fields string
	Status              int
	bts                 []byte // lowercase, excluded from json dump
	BtsDump             string // upper case, is set to an ellipsoid of full sized bts
	Mod                 time.Time
	Msg                 string
	Err                 error
}

// See bts, BtsDump of Job struct
func (j *Job) Bytes() []byte {
	return j.bts
}

// Since json.MarshallIndent cannot
// print http.request channel,
// we have to provide our own stringer
// implementation.
func (j Job) String() string {
	ret := ""
	if j.Err != nil {
		ret += fmt.Sprintf("error was: %v\n", j.Err) // json.MarshallIndent also fails to render certain errors :(
	}
	ret += fmt.Sprintf("   Req %v\n", j.Req.URL)
	ret += fmt.Sprintf("ae Req %v\n", j.AeReq.URL)
	j.Req, j.AeReq = nil, nil
	j.BtsDump = util.Ellipsoider(string(j.bts), 800)
	ret += util.IndentedDump(&j)
	// j.Req, j.AeReq = r1, r2 - no need to restore; it by value anyway
	return ret
}

// We want to inject a warning
// http://choly.ca/post/go-json-marshalling/
func (j *Job) MarshalJSON() ([]byte, error) {
	warning := `jsonifying this type has many problems (channels, missing custom error fields...). 
Use the custom String() method.`
	type Alias Job // prevent recursion
	return json.Marshal(&struct {
		Warning string `json:"InjectedWarning"`
		*Alias
	}{
		Warning: warning,
		Alias:   (*Alias)(j), // prevent recursion
	})
}

// UrlGetter universal http getter for app engine and standalone go programs.
// Previously response was returned. Forgot why. Dropped it.
func (f *Job) Fetch() {

	var err error
	httpsCause := false

	if f.Timeout == 0 {
		f.Timeout = 35
	}

	if f.LogLevel > 0 {
		if f.Req != nil {
			f.Msg += fmt.Sprintf("orig req url: %v\n", f.Req.URL.String())
		} else {
			f.Msg += fmt.Sprintf("orig str url: %v\n", f.URL)
		}
	}

	//
	// Either take provided request
	// Or build one from f.URL
	if f.Req == nil {
		var u *url.URL
		u, f.Err = util.UrlParseImproved(f.URL) // Normalize
		if f.Err != nil {
			return
		}
		f.URL = u.String()

		f.Req, f.Err = http.NewRequest("GET", f.URL, nil)
		if f.Err != nil {
			return
		}
	} else {
		if f.Req.URL.Scheme == "" {
			f.Req.URL.Scheme = "https"
		}
	}

	if f.Req.URL.Path == "" {
		f.Req.URL.Path = "/"
	}

	if len(f.ForceProtocol) > 1 {
		f.ForceProtocol = strings.TrimSuffix(f.ForceProtocol, ":")
		if f.ForceProtocol == "http" || f.ForceProtocol == "https" {
			f.Req.URL.Scheme = f.ForceProtocol
			f.Msg += fmt.Sprintf("Forcing protocol %q\n", f.ForceProtocol)
		}
	}

	//
	// Unify appengine plain http.client
	client := util.HttpClient()

	// We could use logx.IsAppengine()
	var ctx context.Context // try appengine ...
	if f.AeReq != nil {
		func() {
			defer func() {
				rec := recover()
				f.Msg += fmt.Sprintf("appengine panic: %v\n", rec)
			}()
			ctx = appengine.NewContext(f.AeReq)
		}()
	}
	if f.AeReq == nil || ctx == nil {
		client.Timeout = time.Duration(f.Timeout * time.Second) // GAE does not allow that long
		f.Msg += fmt.Sprintf("standard  client\n")
	} else {
		client = urlfetch.Client(ctx)
		f.Msg += fmt.Sprintf("appengine client\n")

		// this does not prevent urlfetch: SSL_CERTIFICATE_ERROR
		// it merely leads to err = "DEADLINE_EXCEEDED"
		tr := urlfetch.Transport{Context: ctx, AllowInvalidServerCertificate: true}
		// thus
		tr = urlfetch.Transport{Context: ctx, AllowInvalidServerCertificate: false}
		// tr.Deadline = f.Timeout * time.Second // only possible on aeOld
		client.Transport = &tr
		client.Timeout = f.Timeout * time.Second // also not in google.golang.org/appengine/urlfetch

		// appengine dev server => always fallback to http
		if appengine.IsDevAppServer() && !f.ForceHttps {
			f.Req.URL.Scheme = "http"
		}
	}

	if f.LogLevel > 0 {
		f.Msg += fmt.Sprintf("url standardized to %v\n", f.Req.URL.String())
	}

	if f.OnRedirect == 1 {
		redirectHandler := func(req *http.Request, via []*http.Request) error {
			if len(via) == 1 && req.URL.Path == via[0].URL.Path+"/" {
				// allow redirect from /gesundheit to /gesundheit/
				return nil
			}
			spath := "\n"
			for _, v := range via {
				spath += v.URL.Path + "\n"
			}
			spath += req.URL.Path + "\n"
			return fmt.Errorf("%v %v", MsgNoRedirects, spath)
		}
		client.CheckRedirect = redirectHandler
	}

	// The actual call
	// =============================
	resp, err := client.Do(f.Req)

	if err != nil {

		if f.OnRedirect == 1 { // Handle redirect error case
			if strings.Contains(err.Error(), MsgNoRedirects) {
				f.Mod = time.Now().Add(-10 * time.Minute)
				f.Msg += "First call failed due to redirect\n"
				f.Err = err
				return
			}
		}

		// Under narrow conditions => fallback to http
		httpsCause = httpsCause || strings.Contains(err.Error(), "SSL_CERTIFICATE_ERROR")
		httpsCause = httpsCause || strings.Contains(err.Error(), "tls: oversized record received with length")

		if httpsCause && f.Req.URL.Scheme == "https" && f.Req.Method == "POST" {
			// We cannot do a fallback for a post request -
			// the r.Body.Reader is consumed
			f.Msg += "Cannot do https requests. Possible reason: Dev server\n"
			if strings.Contains(
				err.Error(),
				"net/http: Client Transport of type init.failingTransport doesn't support CancelRequest; Timeout not supported",
			) {
				f.Msg += "Did you forget to submit the AE Request?\n"
			}
			f.Err = err
			return
		}

		if httpsCause && f.Req.URL.Scheme == "https" && f.Req.Method == "GET" {
			f.Req.URL.Scheme = "http"
			var err2nd error
			resp, err2nd = client.Do(f.Req)
			// while protocol http may go through
			// next obstacle might be - again - a redirect error:
			if err2nd != nil {
				if f.OnRedirect == 1 { // Handle redirect error case
					if strings.Contains(err2nd.Error(), MsgNoRedirects) {
						f.Mod = time.Now().Add(-10 * time.Minute)
						f.Msg += "GET fallback failed due to redirect\n"
						f.Err = err2nd
						return
					}
				}
				f.Msg += fmt.Sprintf("GET fallback to http failed with %v\n", err2nd)
				f.Err = err
				return
			}
			f.Msg += fmt.Sprintf("\tsuccessful fallback to http %v", f.Req.URL.String())
			f.Msg += fmt.Sprintf("\tafter %v\n", err)
			err = nil // CLEAR error
		}
	}

	if err != nil {
		f.Err = err
		return
	}

	//
	// We got response, but
	// explicit bad response from server
	if resp == nil || resp.Body == nil {
		f.Err = fmt.Errorf("resp or resp.Body was nil")
		return
	}

	f.Status = resp.StatusCode

	f.bts, f.Err = ioutil.ReadAll(resp.Body)
	if f.Err != nil {
		return
	}
	defer resp.Body.Close()

	// time stamp
	var tlm time.Time // time last modified
	lm := resp.Header.Get("Last-Modified")
	if lm != "" {
		tlm, err = time.Parse(time.RFC1123, lm) // Last-Modified: Sat, 29 Aug 2015 21:15:39 GMT
		if err != nil {
			tlm, err = time.Parse(time.RFC1123Z, lm) // with numeric time zone
			if err != nil {
				var zeroTime time.Time
				tlm = zeroTime
			}
		}
	}
	f.Mod = tlm

	return

}

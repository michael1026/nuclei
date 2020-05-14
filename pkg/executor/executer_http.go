package executor

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/projectdiscovery/nuclei/pkg/extractors"
	"github.com/projectdiscovery/nuclei/pkg/matchers"
	"github.com/projectdiscovery/nuclei/pkg/requests"
	"github.com/projectdiscovery/nuclei/pkg/templates"
	"github.com/projectdiscovery/retryablehttp-go"
	"golang.org/x/net/proxy"
)

// HTTPExecutor is client for performing HTTP requests
// for a template.
type HTTPExecutor struct {
	httpClient  *retryablehttp.Client
	template    *templates.Template
	httpRequest *requests.HTTPRequest
	writer      *bufio.Writer
	outputMutex *sync.Mutex
}

// HTTPOptions contains configuration options for the HTTP executor.
type HTTPOptions struct {
	Template      *templates.Template
	HTTPRequest   *requests.HTTPRequest
	Writer        *bufio.Writer
	Timeout       int
	Retries       int
	ProxyURL      string
	ProxySocksURL string
}

// NewHTTPExecutor creates a new HTTP executor from a template
// and a HTTP request query.
func NewHTTPExecutor(options *HTTPOptions) (*HTTPExecutor, error) {
	var proxyURL *url.URL
	var err error

	if options.ProxyURL != "" {
		proxyURL, err = url.Parse(options.ProxyURL)
	}
	if err != nil {
		return nil, err
	}

	// Create the HTTP Client
	client := makeHTTPClient(proxyURL, options)
	client.CheckRetry = retryablehttp.HostSprayRetryPolicy()

	executer := &HTTPExecutor{
		httpClient:  client,
		template:    options.Template,
		httpRequest: options.HTTPRequest,
		outputMutex: &sync.Mutex{},
		writer:      options.Writer,
	}
	return executer, nil
}

// ConfigureAutoType makes HTTP request to random URLs to configure what a 404 looks like
func (e *HTTPExecutor) ConfigureAutoType(URL string) error {
	// Create config requests
	compiledConfigRequest, err := e.httpRequest.MakeHTTPRequestForAutoConfigure(URL, 16)
	compiledConfigRequest, err = e.httpRequest.MakeHTTPRequestForAutoConfigure(URL, 32)
	if err != nil {
		return errors.Wrap(err, "could not make auto configure http request")
	}

	for _, matcher := range e.httpRequest.Matchers {
		if matcher.Type == "auto" {
			// create a new matcher here for target
			var m *matchers.Matcher
			m.Target = URL
			e.httpRequest.Matchers = append(e.httpRequest.Matchers, m)

			for _, req := range compiledConfigRequest {
				resp, err := e.httpClient.Do(req)
				if err != nil {
					if resp != nil {
						resp.Body.Close()
					}
					return errors.Wrap(err, "could not make http request")
				}

				data, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					io.Copy(ioutil.Discard, resp.Body)
					resp.Body.Close()
					return errors.Wrap(err, "could not read http body")
				}
				resp.Body.Close()

				// Convert response body from []byte to string with zero copy
				body := unsafeToString(data)

				// Don't add duplicate response sizes
				for _, size := range m.Size {
					if size == len(body) {
						continue
					}
				}

				m.Size = append(m.Size, len(body))
				m.Status = append(m.Status, resp.StatusCode)
			}
		}
	}

	return nil
}

// ExecuteHTTP executes the HTTP request on a URL
func (e *HTTPExecutor) ExecuteHTTP(URL string) error {
	// Compile each request for the template based on the URL
	compiledRequest, err := e.httpRequest.MakeHTTPRequest(URL)
	if err != nil {
		return errors.Wrap(err, "could not make http request")
	}

	e.ConfigureAutoType(URL)

	// Send the request to the target servers
mainLoop:
	for _, req := range compiledRequest {
		resp, err := e.httpClient.Do(req)
		if err != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return errors.Wrap(err, "could not make http request")
		}

		data, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
			return errors.Wrap(err, "could not read http body")
		}
		resp.Body.Close()

		// Convert response body from []byte to string with zero copy
		body := unsafeToString(data)

		var headers string
		matcherCondition := e.httpRequest.GetMatchersCondition()
		for _, matcher := range e.httpRequest.Matchers {
			if matcher.Target != "" && matcher.Target != URL {
				continue
			}
			// Only build the headers string if the matcher asks for it
			part := matcher.GetPart()
			if part == matchers.AllPart || part == matchers.HeaderPart && headers == "" {
				headers = headersToString(resp.Header)
			}

			// Check if the matcher matched
			if !matcher.Match(resp, body, headers) {
				// If the condition is AND we haven't matched, try next request.
				if matcherCondition == matchers.ANDCondition {
					continue mainLoop
				}
			} else {
				// If the matcher has matched, and its an OR
				// write the first output then move to next matcher.
				if matcherCondition == matchers.ORCondition && len(e.httpRequest.Extractors) == 0 {
					e.writeOutputHTTP(req, matcher, nil)
				}
			}
		}

		// All matchers have successfully completed so now start with the
		// next task which is extraction of input from matchers.
		var extractorResults []string
		for _, extractor := range e.httpRequest.Extractors {
			part := extractor.GetPart()
			if part == extractors.AllPart || part == extractors.HeaderPart && headers == "" {
				headers = headersToString(resp.Header)
			}
			for match := range extractor.Extract(body, headers) {
				extractorResults = append(extractorResults, match)
			}
		}

		// Write a final string of output if matcher type is
		// AND or if we have extractors for the mechanism too.
		if len(e.httpRequest.Extractors) > 0 || matcherCondition == matchers.ANDCondition {
			e.writeOutputHTTP(req, nil, extractorResults)
		}
	}
	return nil
}

// Close closes the http executor for a template.
func (e *HTTPExecutor) Close() {
	e.outputMutex.Lock()
	e.writer.Flush()
	e.outputMutex.Unlock()
}

// makeHTTPClient creates a http client
func makeHTTPClient(proxyURL *url.URL, options *HTTPOptions) *retryablehttp.Client {
	retryablehttpOptions := retryablehttp.DefaultOptionsSpraying
	retryablehttpOptions.RetryWaitMax = 10 * time.Second
	retryablehttpOptions.RetryMax = options.Retries
	followRedirects := options.HTTPRequest.Redirects
	maxRedirects := options.HTTPRequest.MaxRedirects

	transport := &http.Transport{
		MaxIdleConnsPerHost: -1,
		TLSClientConfig: &tls.Config{
			Renegotiation:      tls.RenegotiateOnceAsClient,
			InsecureSkipVerify: true,
		},
		DisableKeepAlives: true,
	}

	// Attempts to overwrite the dial function with the socks proxied version
	if options.ProxySocksURL != "" {
		var proxyAuth *proxy.Auth
		socksURL, err := url.Parse(options.ProxySocksURL)
		if err == nil {
			proxyAuth = &proxy.Auth{}
			proxyAuth.User = socksURL.User.Username()
			proxyAuth.Password, _ = socksURL.User.Password()
		}
		dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("%s:%s", socksURL.Hostname(), socksURL.Port()), proxyAuth, proxy.Direct)
		if err == nil {
			transport.Dial = dialer.Dial
		}
	}

	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return retryablehttp.NewWithHTTPClient(&http.Client{
		Transport:     transport,
		Timeout:       time.Duration(options.Timeout) * time.Second,
		CheckRedirect: makeCheckRedirectFunc(followRedirects, maxRedirects),
	}, retryablehttpOptions)
}

type checkRedirectFunc func(_ *http.Request, requests []*http.Request) error

func makeCheckRedirectFunc(followRedirects bool, maxRedirects int) checkRedirectFunc {
	return func(_ *http.Request, requests []*http.Request) error {
		if !followRedirects {
			return http.ErrUseLastResponse
		}
		if maxRedirects == 0 {
			if len(requests) > 10 {
				return http.ErrUseLastResponse
			}
			return nil
		}
		if len(requests) > maxRedirects {
			return http.ErrUseLastResponse
		}
		return nil
	}
}

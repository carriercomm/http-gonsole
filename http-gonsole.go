// Speak HTTP like a local -- a simple, intuitive HTTP console
// This is a port of http://github.com/cloudhead/http-console

package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"github.com/mattn/go-colorable"
	"github.com/peterh/liner"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	colors          = flag.Bool("colors", true, "colorful output")
	useSSL          = flag.Bool("ssl", false, "use SSL")
	useJSON         = flag.Bool("json", false, "use JSON")
	rememberCookies = flag.Bool("cookies", false, "remember cookies")
	verbose         = flag.Bool("v", false, "be verbose, print out the request in wire format before sending")
	out             = colorable.NewColorableStdout()
)

// Color scheme, ref: http://linuxgazette.net/issue65/padala.html
const (
	C_Prompt = "\x1b[90m"
	C_Header = "\x1b[1m"
	C_2xx    = "\x1b[1;32m"
	C_3xx    = "\x1b[1;36m"
	C_4xx    = "\x1b[1;31m"
	C_5xx    = "\x1b[1;37;41m"
	C_Reset  = "\x1b[0m"
)

func colorize(color, s string) string {
	return color + s + C_Reset
}

type myCloser struct {
	io.Reader
}

func (myCloser) Close() error { return nil }

type Cookie struct {
	Items    map[string]string
	path     string
	expires  time.Time
	domain   string
	secure   bool
	httpOnly bool
}

type Session struct {
	scheme  string
	host    string
	conn    *httputil.ClientConn
	headers http.Header
	cookies *[]*Cookie
	path    *string
}

func dial(host string) (conn *httputil.ClientConn) {
	var tcp net.Conn
	var err error
	fmt.Fprintf(os.Stderr, "http-gonsole: establishing a TCP connection ...\n")
	proxy := os.Getenv("HTTP_PROXY")
	if strings.Split(host, ":")[0] != "localhost" && len(proxy) > 0 {
		proxy_url, _ := url.Parse(proxy)
		tcp, err = net.Dial("tcp", proxy_url.Host)
	} else {
		tcp, err = net.Dial("tcp", host)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "http-gonsole:", err)
		os.Exit(1)
	}
	if *useSSL {
		if len(proxy) > 0 {
			connReq := &http.Request{
				Method: "CONNECT",
				URL:    &url.URL{Path: host},
				Host:   host,
				Header: make(http.Header),
			}
			connReq.Write(tcp)
			resp, err := http.ReadResponse(bufio.NewReader(tcp), connReq)
			if resp.StatusCode != 200 {
				fmt.Fprintln(os.Stderr, "http-gonsole:", resp.Status)
				os.Exit(1)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "http-gonsole:", err)
				os.Exit(1)
			}
			tcp = tls.Client(tcp, nil)
			conn = httputil.NewClientConn(tcp, nil)
		} else {
			tcp = tls.Client(tcp, nil)
			conn = httputil.NewClientConn(tcp, nil)
		}
		if err = tcp.(*tls.Conn).Handshake(); err != nil {
			fmt.Fprintln(os.Stderr, "http-gonsole:", err)
			os.Exit(1)
		}
		if err = tcp.(*tls.Conn).VerifyHostname(strings.Split(host, ":")[0]); err != nil {
			fmt.Fprintln(os.Stderr, "http-gonsole:", err)
			os.Exit(1)
		}
	} else {
		conn = httputil.NewClientConn(tcp, nil)
	}
	return
}

func (s Session) perform(method, uri, data string) {
	var req http.Request
	req.URL, _ = url.Parse(uri)
	req.Method = method
	req.Header = s.headers
	req.ContentLength = int64(len([]byte(data)))
	req.Body = myCloser{bytes.NewBufferString(data)}
	if *verbose {
		req.Write(os.Stderr)
	}
	retry := 0
request:
	req.Body = myCloser{bytes.NewBufferString(data)} // recreate anew, in case of retry
	err := s.conn.Write(&req)
	if err != nil {
		if retry < 2 {
			if err == io.ErrUnexpectedEOF {
				// the underlying connection has been closed "gracefully"
				retry++
				s.conn.Close()
				s.conn = dial(s.host)
				goto request
			} else if protoerr, ok := err.(*http.ProtocolError); ok && protoerr == httputil.ErrPersistEOF {
				// the connection has been closed in an HTTP keepalive sense
				retry++
				s.conn.Close()
				s.conn = dial(s.host)
				goto request
			}
		}
		fmt.Fprintln(os.Stderr, "http-gonsole: could not send request:", err)
		os.Exit(1)
	}
	r, err := s.conn.Read(&req)
	if err != nil {
		if protoerr, ok := err.(*http.ProtocolError); ok && protoerr == httputil.ErrPersistEOF {
			// the remote requested that this be the last request serviced,
			// we proceed as the response is still valid
			defer s.conn.Close()
			defer func() { s.conn = dial(s.host) }()
			goto output
		}
		fmt.Fprintln(os.Stderr, "http-gonsole: could not read response:", err)
		os.Exit(1)
	}
output:
	if len(data) > 0 {
		fmt.Println()
	}
	if r.StatusCode >= 500 {
		fmt.Fprintf(out, colorize(C_5xx, "%s %s\n"), r.Proto, r.Status)
	} else if r.StatusCode >= 400 {
		fmt.Fprintf(out, colorize(C_4xx, "%s %s\n"), r.Proto, r.Status)
	} else if r.StatusCode >= 300 {
		fmt.Fprintf(out, colorize(C_3xx, "%s %s\n"), r.Proto, r.Status)
	} else if r.StatusCode >= 200 {
		fmt.Fprintf(out, colorize(C_2xx, "%s %s\n"), r.Proto, r.Status)
	}
	if len(r.Header) > 0 {
		for key, arr := range r.Header {
			for _, val := range arr {
				fmt.Fprintf(out, colorize(C_Header, "%s: "), key)
				fmt.Println(val)
			}
		}
		fmt.Println()
	}
	if *rememberCookies {
		if cookies, found := r.Header["Set-Cookie"]; found {
			for _, h := range cookies {
				cookie := new(Cookie)
				cookie.Items = map[string]string{}
				re, _ := regexp.Compile("^[^=]+=[^;]+(; *(expires=[^;]+|path=[^;,]+|domain=[^;,]+|secure))*,?")
				rs := re.FindAllString(h, -1)
				for _, ss := range rs {
					m := strings.Split(ss, ";")
					for _, n := range m {
						t := strings.SplitN(n, "=", 2)
						if len(t) == 2 {
							t[0] = strings.Trim(t[0], " ")
							t[1] = strings.Trim(t[1], " ")
							switch t[0] {
							case "domain":
								cookie.domain = t[1]
							case "path":
								cookie.path = t[1]
							case "expires":
								tm, err := time.Parse("Fri, 02-Jan-2006 15:04:05 MST", t[1])
								if err != nil {
									tm, err = time.Parse("Fri, 02-Jan-2006 15:04:05 -0700", t[1])
								}
								cookie.expires = tm
							case "secure":
								cookie.secure = true
							case "HttpOnly":
								cookie.httpOnly = true
							default:
								cookie.Items[t[0]] = t[1]
							}
						}
					}
				}
				*s.cookies = append(*s.cookies, cookie)
			}
		}
	}
	h := r.Header.Get("Content-Length")
	if len(h) > 0 {
		n, _ := strconv.ParseInt(h, 10, 64)
		b := make([]byte, n)
		io.ReadFull(r.Body, b)
		fmt.Println(string(b))
	} else if method != "HEAD" {
		b, _ := ioutil.ReadAll(r.Body)
		fmt.Println(string(b))
	} else {
		// TODO: streaming?
	}
}

// Parse a single command and execute it. (REPL without the loop)
// Return true when the quit command is given.
func (s Session) repl() bool {
	var prompt string
	if runtime.GOOS == "windows" {
		prompt = fmt.Sprintf("%s://%s%s: ", s.scheme, s.host, *s.path)
	} else {
		prompt = fmt.Sprintf(colorize(C_Prompt, "%s://%s%s: "), s.scheme, s.host, *s.path)
	}
	var err error
	var line string
	ln := liner.NewLiner()
	defer ln.Close()
	for {
		line, err = ln.Prompt(prompt)
		if err != nil {
			fmt.Println()
			return true
		}
		line = strings.Trim(line, "\n")
		line = strings.Trim(line, "\r")
		if line != "" {
			break
		}
	}
	if match, _ := regexp.MatchString("^(/[^ \t]*)|(\\.\\.)$", line); match {
		if line == "/" || line == "//" {
			*s.path = "/"
		} else {
			*s.path = strings.Replace(path.Clean(path.Join(*s.path, line)), "\\", "/", -1)
			if len(line) > 1 && line[len(line)-1] == '/' {
				*s.path += "/"
			}
		}
		return false
	}
	re := regexp.MustCompile("^([a-zA-Z][a-zA-Z0-9\\-]+):(.*)")
	if match := re.FindStringSubmatch(line); match != nil {
		key := match[1]
		val := strings.TrimSpace(match[2])
		if len(val) > 0 {
			s.headers.Set(key, val)
		}
		return false
	}
	re = regexp.MustCompile("^([A-Z]+)(.*)")
	if match := re.FindStringSubmatch(line); match != nil {
		method := match[1]
		p := strings.TrimSpace(match[2])
		trailingSlash := (len(*s.path) > 1) && ((*s.path)[len(*s.path)-1] == '/')
		if len(p) == 0 {
			p = "/"
		} else {
			trailingSlash = p[len(p)-1] == '/'
		}
		p = strings.Replace(path.Clean(path.Join(*s.path, p)), "\\", "/", -1)
		if trailingSlash {
			p += "/"
		}
		data := ""
		if method == "POST" || method == "PUT" {
			prompt = colorize(C_Prompt, "...: ")
			line, err = ln.Prompt(prompt)
			if line == "" {
				return false
			}
		}
		ln.AppendHistory(line)
		s.perform(method, s.scheme+"://"+s.host+p, data)
		return false
	}
	if line == ".h" || line == ".headers" {
		for key, arr := range s.headers {
			for _, val := range arr {
				fmt.Println(key + ": " + val)
			}
		}
		return false
	}
	if line == ".c" || line == ".cookies" {
		for _, cookie := range *s.cookies {
			for key, val := range cookie.Items {
				fmt.Println(key + ": " + val)
			}
		}
		return false
	}
	if line == ".v" || line == ".verbose" {
		*verbose = !*verbose
		return false
	}
	if line == ".o" || line == ".options" {
		fmt.Printf("useSSL=%v, rememberCookies=%v, verbose=%v\n", *useSSL, *rememberCookies, *verbose)
		return false
	}
	if line == ".?" || line == ".help" {
		fmt.Println(".headers, .h    show active request headers\n" +
			".options, .o    show options\n" +
			".cookies, .c    show client cookies\n" +
			".help, .?       display this message\n" +
			".exit, .q, ^D   exit console\n")
		return false
	}
	if line == ".q" || line == ".exit" {
		return true
	}
	fmt.Fprintln(os.Stderr, "unknown command:", line)
	return false
}

func main() {
	scheme := "http"
	host := "localhost:80"
	headers := make(http.Header)
	cookies := new([]*Cookie)
	p := "/"
	flag.Parse()
	if flag.NArg() > 0 {
		tmp := flag.Arg(0)
		if match, _ := regexp.MatchString("^[^:]+(:[0-9]+)?$", tmp); match {
			tmp = "http://" + tmp
		}
		targetURL, err := url.Parse(tmp)
		if err != nil {
			fmt.Fprintln(os.Stderr, "malformed URL")
			os.Exit(-1)
		}
		host = targetURL.Host
		if len(host) == 0 {
			fmt.Fprintln(os.Stderr, "invalid host name")
			os.Exit(-1)
		}
		if *useSSL || targetURL.Scheme == "https" {
			*useSSL = true
			scheme = "https"
		}
		if match, _ := regexp.MatchString("^[^:]+:[0-9]+$", host); !match {
			if *useSSL {
				host = host + ":443"
			} else {
				host = host + ":80"
			}
		}
		scheme = targetURL.Scheme
		if info := targetURL.User; info != nil {
			enc := base64.URLEncoding
			encoded := make([]byte, enc.EncodedLen(len(info.String())))
			enc.Encode(encoded, []byte(info.String()))
			headers.Set("Authorization", "Basic "+string(encoded))
		}
		p = strings.Replace(path.Clean(targetURL.Path), "\\", "/", -1)
		if p == "." {
			p = "/"
		}
	} else if *useSSL {
		scheme = "https"
		host = "localhost:443"
	}
	headers.Set("Host", host)
	session := &Session{
		scheme:  scheme,
		host:    host,
		conn:    dial(host),
		headers: headers,
		cookies: cookies,
		path:    &p,
	}

	if *useJSON {
		headers.Set("Accept", "*/*")
		headers.Set("Content-Type", "appliaction/json")
	}

	defer session.conn.Close()
	done := false
	for !done {
		done = session.repl()
	}
}

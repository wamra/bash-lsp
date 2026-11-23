package workspace

import (
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"strings"
)

// PathFromURI converts a file:// URI to an OS path. Only file URIs are
// accepted. On Windows "/C:/x" becomes `C:\x` and a host names a UNC share.
func PathFromURI(uri string) (string, error) { return pathFromURI(uri, runtime.GOOS == "windows") }

// URIFromPath converts an absolute OS path to a file:// URI.
func URIFromPath(path string) string { return uriFromPath(path, runtime.GOOS == "windows") }

func pathFromURI(uri string, windows bool) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Scheme, "file") {
		return "", fmt.Errorf("not a file URI: %q", uri)
	}
	p := u.Path
	if p == "" {
		return "", errors.New("file URI has no path: " + uri)
	}
	host := u.Host
	if strings.EqualFold(host, "localhost") {
		host = ""
	}
	if !windows {
		if host != "" {
			return "", fmt.Errorf("file URI names a remote host: %q", uri)
		}
		return p, nil
	}
	if host != "" {
		return `\\` + host + strings.ReplaceAll(p, "/", `\`), nil
	}
	if len(p) >= 3 && p[0] == '/' && isLetter(p[1]) && p[2] == ':' {
		p = strings.ToUpper(p[1:2]) + p[2:]
	}
	return strings.ReplaceAll(p, "/", `\`), nil
}

func uriFromPath(path string, windows bool) string {
	u := url.URL{Scheme: "file"}
	if windows {
		p := strings.ReplaceAll(path, `\`, "/")
		switch {
		case strings.HasPrefix(p, "//"):
			rest := p[2:]
			host, share, _ := strings.Cut(rest, "/")
			u.Host, u.Path = host, "/"+share
		case len(p) >= 2 && isLetter(p[0]) && p[1] == ':':
			u.Path = "/" + strings.ToUpper(p[:1]) + p[1:]
		default:
			u.Path = p
		}
	} else {
		u.Path = path
	}
	return u.String()
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

package proxy

import (
	"bytes"
	"fmt"
	"html"
	"mime"
	"strconv"
	"strings"

	"github.com/btnguyen2k/cheeseshop-proxy/internal/upstream"
)

func negotiate(accept string) (string, bool) {
	if strings.TrimSpace(accept) == "" {
		return upstream.HTMLType, true
	}
	type mediaRange struct {
		name    string
		quality float64
	}
	var ranges []mediaRange
	for _, entry := range strings.Split(accept, ",") {
		name, parameters, err := mime.ParseMediaType(strings.TrimSpace(entry))
		if err != nil {
			continue
		}
		quality := 1.0
		if raw, ok := parameters["q"]; ok {
			quality, err = strconv.ParseFloat(raw, 64)
			if err != nil || !(quality >= 0 && quality <= 1) {
				continue
			}
		}
		name = strings.Replace(name, "application/vnd.pypi.simple.latest+", "application/vnd.pypi.simple.v1+", 1)
		ranges = append(ranges, mediaRange{name: name, quality: quality})
	}
	selected, bestQuality := "", 0.0
	for _, candidate := range []string{upstream.HTMLType, "text/html", upstream.JSONType} {
		quality, specificity := 0.0, -1
		for _, item := range ranges {
			match := -1
			switch {
			case item.name == candidate:
				match = 2
			case item.name == strings.SplitN(candidate, "/", 2)[0]+"/*":
				match = 1
			case item.name == "*/*":
				match = 0
			}
			if match >= 0 && (match > specificity || (match == specificity && item.quality > quality)) {
				quality, specificity = item.quality, match
			}
		}
		if quality > bestQuality {
			selected, bestQuality = candidate, quality
		}
	}
	return selected, selected != ""
}

func htmlHeader(buffer *bytes.Buffer, title, version string) {
	fmt.Fprintf(buffer, "<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><meta name=\"pypi:repository-version\" content=\"%s\"><title>%s</title></head><body>\n",
		html.EscapeString(version), html.EscapeString(title))
}

func (s *Server) projectListHTML(document projectList) []byte {
	var buffer bytes.Buffer
	htmlHeader(&buffer, "cheeseshop", document.Meta.Version)
	for _, project := range document.Projects {
		fmt.Fprintf(&buffer, "<a href=\"%s\">%s</a>\n",
			html.EscapeString(s.publicURL("/simple/"+project.Name+"/")), html.EscapeString(project.Name))
	}
	buffer.WriteString("</body></html>\n")
	return buffer.Bytes()
}

func preferredHash(hashes map[string]string) (string, string) {
	for _, algorithm := range []string{"sha256", "sha512", "sha384", "sha224", "sha1", "md5"} {
		if digest := hashes[algorithm]; digest != "" {
			return algorithm, digest
		}
	}
	return "", ""
}

func metadataAttribute(value any) string {
	switch typed := value.(type) {
	case bool:
		if typed {
			return "true"
		}
	case map[string]string:
		if algorithm, digest := preferredHash(typed); algorithm != "" {
			return algorithm + "=" + digest
		}
		if len(typed) > 0 {
			return "true"
		}
	}
	return ""
}

func projectHTML(document projectDocument) []byte {
	var buffer bytes.Buffer
	htmlHeader(&buffer, document.Name, document.Meta.Version)
	for _, file := range document.Files {
		href := file.URL
		if algorithm, digest := preferredHash(file.Hashes); algorithm != "" {
			href += "#" + algorithm + "=" + digest
		}
		fmt.Fprintf(&buffer, "<a href=\"%s\"", html.EscapeString(href))
		if file.RequiresPython != nil {
			fmt.Fprintf(&buffer, " data-requires-python=\"%s\"", html.EscapeString(*file.RequiresPython))
		}
		if metadata := metadataAttribute(file.CoreMetadata); metadata != "" {
			fmt.Fprintf(&buffer, " data-core-metadata=\"%s\" data-dist-info-metadata=\"%s\"",
				html.EscapeString(metadata), html.EscapeString(metadata))
		}
		switch value := file.Yanked.(type) {
		case bool:
			if value {
				buffer.WriteString(" data-yanked=\"\"")
			}
		case string:
			fmt.Fprintf(&buffer, " data-yanked=\"%s\"", html.EscapeString(value))
		}
		fmt.Fprintf(&buffer, ">%s</a>\n", html.EscapeString(file.Filename))
	}
	buffer.WriteString("</body></html>\n")
	return buffer.Bytes()
}

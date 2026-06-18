package proxy

import (
	"fmt"
	"strings"
)

type RouteInfo struct {
	Upstream  string
	Namespace string
	Image     string
	Tag       string
	Digest    string
}

func ParseRoute(path string) (*RouteInfo, error) {
	if strings.HasPrefix(path, "/v2/") {
		path = path[4:]
	}

	var imagePart string

	if strings.Contains(path, "/manifests/") {
		parts := strings.Split(path, "/manifests/")
		imagePart = parts[0]
	} else if strings.Contains(path, "/blobs/") {
		parts := strings.Split(path, "/blobs/")
		imagePart = parts[0]
	} else {
		return nil, fmt.Errorf("invalid route: %s", path)
	}

	imageParts := strings.Split(imagePart, "/")
	if len(imageParts) < 2 {
		return nil, fmt.Errorf("invalid image format: %s", imagePart)
	}

	upstream := imageParts[0]
	image := imageParts[len(imageParts)-1]

	return &RouteInfo{
		Upstream: upstream,
		Image:    image,
		Tag:      "latest",
	}, nil
}

func (r *RouteInfo) GetFullPath() string {
	return r.Upstream + "/" + r.Image
}

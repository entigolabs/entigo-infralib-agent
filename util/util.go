package util

import (
	"bytes"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/hashicorp/go-version"
	"io"
	"net/http"
)

func CreateKeyValuePairs(m map[string]string, prefix string, suffix string) ([]byte, error) {
	b := new(bytes.Buffer)
	if prefix != "" {
		b.Write([]byte(prefix))
	}
	for key, value := range m {
		_, err := fmt.Fprintf(b, "%s=\"%s\"\n", key, value)
		if err != nil {
			return nil, err
		}
	}
	if suffix != "" {
		b.Write([]byte(suffix))
	}
	return bytes.TrimRight(b.Bytes(), ", "), nil
}

func MapValues[M ~map[K]V, K comparable, V any](m M) []V {
	r := make([]V, 0, len(m))
	for _, v := range m {
		r = append(r, v)
	}
	return r
}

func GetNewestVersion(versions []string) (string, error) {
	firstVersions, otherVersions := versions[0], versions[1:]
	newestVersionSemver, err := version.NewVersion(firstVersions)
	if err != nil {
		return "", err
	}
	for _, ver := range otherVersions {
		versionSemver, err := version.NewVersion(ver)
		if err != nil {
			return "", err
		}
		if versionSemver.GreaterThan(newestVersionSemver) {
			newestVersionSemver = versionSemver
		}
	}
	return newestVersionSemver.Original(), nil
}

func GetFileFromUrl(fileUrl string) ([]byte, error) {
	resp, err := http.Get(fileUrl)
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			common.Logger.Printf("Failed to close response body: %s", err)
		}
	}(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

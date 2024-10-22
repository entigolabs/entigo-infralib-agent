package util

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/hashicorp/go-version"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
			log.Printf("Failed to close response body: %s", err)
		}
	}(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func ToList(value string) []string {
	if value == "" || value == "[]" {
		return []string{}
	}
	quotes := ""
	if strings.Contains(value, "\"") {
		quotes = "\""
	}
	value = strings.Trim(value, "\n")
	value = strings.TrimPrefix(value, fmt.Sprintf("[%s", quotes))
	value = strings.TrimSuffix(value, fmt.Sprintf("%s]", quotes))
	return strings.Split(value, fmt.Sprintf("%s,%s", quotes, quotes))
}

func EqualLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func GetValueFromStruct(keyWithDots string, object interface{}) (string, error) {
	keySlice := strings.Split(keyWithDots, ".")
	v := reflect.ValueOf(object)
	for _, key := range keySlice {
		key = strings.ReplaceAll(key, "_", " ")
		key = cases.Title(language.English, cases.Compact).String(key)
		key = strings.ReplaceAll(key, " ", "")
		for v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return "", fmt.Errorf("only accepts structs; got %T", v)
		}
		v = v.FieldByName(key)
	}
	if v.Kind() != reflect.String {
		return "", fmt.Errorf("found value with key %s is not a string, got %T", keyWithDots, v)
	}
	return v.String(), nil
}

func YamlBytesToMap(b []byte) (map[string]interface{}, error) {
	if len(b) == 0 {
		return nil, nil
	}
	m := make(map[string]interface{})
	err := yaml.Unmarshal(b, &m)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func MapToYamlBytes(m map[string]interface{}) ([]byte, error) {
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func IsClientModule(module model.Module) bool {
	return strings.HasPrefix(module.Source, "git::") || strings.HasPrefix(module.Source, "git@")
}

func TarGzWrite(inDirPath string) ([]byte, error) {
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	tw := tar.NewWriter(gw)

	err := filepath.Walk(inDirPath, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Return on non-regular files. We don't add directories without files and symlinks
		if !fi.Mode().IsRegular() {
			return nil
		}
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}
		header.Name = filepath.Join(filepath.Base(inDirPath), strings.TrimPrefix(file, inDirPath))
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		f, err := os.Open(file)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		f.Close()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	if err = gw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func MinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

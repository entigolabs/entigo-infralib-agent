package util

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
	"hash/fnv"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

func CreateKeyValuePairs(m map[string]string, prefix string, suffix string) ([]byte, error) {
	b := new(bytes.Buffer)
	if prefix != "" {
		b.Write([]byte(prefix))
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := m[key]
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

func IsLocalSource(source string) bool {
	return !strings.HasPrefix(source, "http:") && !strings.HasPrefix(source, "https:")
}

func FileExists(source, path string) bool {
	fullPath := filepath.Join(source, path)
	info, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
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
		header.Name = filepath.Join(filepath.Base(inDirPath), fi.Name())
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
		_ = f.Close()
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

func MarshalYamlWithJsonTags(v interface{}) ([]byte, error) {
	jsonBytes, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var jsonObj interface{}
	err = yaml.Unmarshal(jsonBytes, &jsonObj)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(jsonObj)
}

func SetChildStringValue(data map[string]interface{}, newValue string, overwrite bool, keys ...string) error {
	for i, key := range keys {
		if i == len(keys)-1 {
			_, exists := data[key]
			if !exists || overwrite {
				data[key] = newValue
			}
			break
		}
		val, exists := data[key]
		if !exists {
			newval := make(map[string]interface{})
			data[key] = newval
			data = newval
			continue
		}
		var ok bool
		data, ok = val.(map[string]interface{})
		if !ok {
			return fmt.Errorf("value of key %s is not a map[string]interface{}", key)
		}
	}
	return nil
}

func DelayBucketCreation(bucket string, skipDelay bool) {
	slog.Warn(common.PrefixWarning(fmt.Sprintf("Bucket %s doesn't exist", bucket)))
	if skipDelay {
		return
	}
	done := make(chan bool)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		_, _ = reader.ReadString('\n')
		done <- true
	}()
	fmt.Println("Waiting for 10 seconds before creating the bucket, press Enter to skip...")
	select {
	case <-time.After(10 * time.Second):
		return
	case <-done:
		return
	}
}

func AskForConfirmation() error {
	for {
		reader := bufio.NewReader(os.Stdin)
		response, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read input: %w", err)
		}
		response = strings.ToLower(strings.TrimSpace(response))
		if response == "y" || response == "yes" {
			return nil
		} else if response == "n" || response == "no" {
			return fmt.Errorf("operation cancelled")
		} else {
			slog.Warn(common.PrefixWarning("Invalid input. Please enter Y or N."))
		}
	}
}

func CalculateHash(content []byte) []byte {
	hash := sha256.New()
	hash.Write(content)
	return hash.Sum(nil)
}

func SortKeys(data interface{}) interface{} {
	switch v := data.(type) {
	case map[interface{}]interface{}:
		sorted := make(map[interface{}]interface{})
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key.(string))
		}
		sort.Strings(keys)
		for _, key := range keys {
			sorted[key] = SortKeys(v[key])
		}
		return sorted
	case []interface{}:
		for i, item := range v {
			v[i] = SortKeys(item)
		}
	}
	return data
}

func HashCode(value string) string {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(value))
	return strings.ToLower(fmt.Sprintf("%x", hasher.Sum32()))
}

func GetOutputStringValue(outputs map[string]model.TFOutput, key string) (string, error) {
	value, err := GetOutputValue(outputs, key)
	if err != nil {
		return "", fmt.Errorf("failed to get output %s value: %v", key, err)
	}
	if value == nil {
		return "", nil
	}
	return *value, nil
}

func GetOutputValue(outputs map[string]model.TFOutput, key string) (*string, error) {
	output, found := outputs[key]
	if !found {
		return nil, nil
	}
	var value string
	switch v := output.Value.(type) {
	case string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, bool:
		value = strings.Trim(GetStringValue(v), "\"")
	case []interface{}:
		values := make([]string, 0)
		for _, value := range v {
			values = append(values, GetStringValue(value))
		}
		value = strings.Join(values, ",")
	case map[string]interface{}:
		slog.Warn(common.PrefixWarning(fmt.Sprintf("tf output %s is a map, returning as json", output.Type)))
		jsonBytes, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		value = string(jsonBytes)
	default:
		return nil, fmt.Errorf("unsupported type: %s", reflect.TypeOf(output.Value))
	}
	return &value, nil
}

func GetStringValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return fmt.Sprintf(`"%s"`, v)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v)
	case float32, float64:
		return fmt.Sprintf("%f", v)
	case bool:
		return fmt.Sprintf("%t", v)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func ShouldStopPipeline(changes model.PipelineChanges, approve model.Approve, manualApprove model.ManualApprove) bool {
	return approve == model.ApproveReject || manualApprove == model.ManualApproveReject || changes.NoChanges
}

func ShouldApprovePipeline(changes model.PipelineChanges, approve model.Approve, autoApprove bool, manualApprove model.ManualApprove) bool {
	if approve == model.ApproveForce || manualApprove == model.ManualApproveNever {
		return true
	}
	if changes.Added == 0 && changes.Changed == 0 && changes.Destroyed == 0 {
		return true
	}
	if manualApprove != "" {
		switch manualApprove {
		case model.ManualApproveAlways:
			return false
		case model.ManualApproveChanges:
			return changes.Changed == 0 && changes.Destroyed == 0
		case model.ManualApproveRemoves:
			return changes.Destroyed == 0
		}
	}
	return changes.Destroyed == 0 && (changes.Changed == 0 || autoApprove)
}

func GetChangesFromMatches(pipelineName, message string, matches []string) (*model.PipelineChanges, error) {
	log.Printf("Pipeline %s: %s", pipelineName, message)
	changes := model.PipelineChanges{}
	added := matches[1]
	changed := matches[2]
	destroyed := matches[3]
	if added == "0" && changed == "0" && destroyed == "0" {
		changes.NoChanges = true
		return &changes, nil
	}
	var err error
	changes.Added, err = strconv.Atoi(added)
	if err != nil {
		return nil, err
	}
	changes.Changed, err = strconv.Atoi(changed)
	if err != nil {
		return nil, err
	}
	changes.Destroyed, err = strconv.Atoi(destroyed)
	if err != nil {
		return nil, err
	}
	return &changes, nil
}

func FormatChanges(changes model.PipelineChanges) string {
	return fmt.Sprintf("Plan: %d to add, %d to change, %d to destroy.", changes.Added, changes.Changed, changes.Destroyed)
}

func Notify(notifiers []model.Notifier, message string) {
	for _, notifier := range notifiers {
		go func() {
			err := notifier.Notify(message)
			if err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to notify %s: %v", notifier.GetName(), err)))
			}
		}()
	}
}

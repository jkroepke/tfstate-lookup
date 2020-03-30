package tfstate

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/itchyny/gojq"
	"github.com/pkg/errors"
)

var (
	defaultWorkspace          = "default"
	defaultWorkspeceKeyPrefix = "env:"
)

type Object struct {
	Value interface{}
}

func (a Object) String() string {
	switch v := a.Value; v.(type) {
	case string, float64:
		return fmt.Sprint(v)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// Query queries object by go-jq
func (a *Object) Query(query string) (*Object, error) {
	jq, err := gojq.Parse(query)
	if err != nil {
		return nil, err
	}
	iter := jq.Run(a.Value)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			return nil, err
		}
		return &Object{v}, nil
	}
	return &Object{}, nil // not found
}

// TFState represents a tfstate
type TFState struct {
	state tfstate
}

type tfstate struct {
	Resources []interface{} `json:"resources"`
	Backend   *backend      `json:"backend"`
}

type backend struct {
	Type   string `json:"type"`
	Config map[string]*string
}

// Read reads a tfstate from io.Reader
func Read(src io.Reader) (*TFState, error) {
	return ReadWithWorkspace(src, defaultWorkspace)
}

// ReadWithWorkspace reads a tfstate from io.Reader with workspace
func ReadWithWorkspace(src io.Reader, ws string) (*TFState, error) {
	if ws == "" {
		ws = defaultWorkspace
	}
	var s TFState
	if err := json.NewDecoder(src).Decode(&s.state); err != nil {
		return nil, errors.Wrap(err, "invalid json")
	}
	if s.state.Backend != nil {
		remote, err := readRemoteState(s.state.Backend, ws)
		if err != nil {
			return nil, err
		}
		defer remote.Close()
		return Read(remote)
	}
	return &s, nil
}

// ReadFile reads terraform.tfstate from the file (a workspace reads from environment file in the same directory)
func ReadFile(file string) (*TFState, error) {
	ws, _ := ioutil.ReadFile(filepath.Join(filepath.Dir(file), "environment"))
	// if not exist, don't care (using default workspace)

	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadWithWorkspace(f, string(ws))
}

// Lookup lookups attributes of the specified key in tfstate
func (s *TFState) Lookup(key string) (*Object, error) {
	resQuery, attrQuery, err := parseAddress(key)
	if err != nil {
		return nil, err
	}

	attr, err := (&Object{s.state.Resources}).Query(resQuery)
	if err != nil {
		return nil, err
	}
	return attr.Query(attrQuery)
}

func parseAddress(key string) (string, string, error) {
	parts := strings.Split(key, ".")
	if len(parts) < 2 ||
		parts[0] == "module" && len(parts) < 4 ||
		parts[0] == "data" && len(parts) < 3 {
		return "", "", fmt.Errorf("invalid address: %s", key)
	}

	resq := []string{".[]"}
	var query string
	if parts[0] == "module" {
		resq = append(resq, fmt.Sprintf(`select(.module == "module.%s")`, parts[1]))
		parts = parts[2:] // remove module prefix
	}

	if parts[0] == "data" {
		resq = append(resq, fmt.Sprintf(
			`select(.mode == "data" and .type == "%s" and .name == "%s").instances[0].attributes`,
			parts[1], parts[2],
		))
		query = "." + strings.Join(parts[3:], ".")
	} else {
		n := parts[1] // foo["bar"], foo[0]

		if i := strings.Index(n, "["); i != -1 { // each
			indexKey := n[i+1 : len(n)-1] // "bar", 0
			name := n[0:i]                // foo
			resq = append(resq, fmt.Sprintf(
				`select(.mode == "managed" and .type == "%s" and .name == "%s").instances[] | select(.index_key == %s).attributes`,
				parts[0], name, indexKey,
			))
		} else {
			resq = append(resq, fmt.Sprintf(
				`select(.mode == "managed" and .type == "%s" and .name == "%s" and .each == null).instances[0].attributes`,
				parts[0], parts[1],
			))
		}
		query = "." + strings.Join(parts[2:], ".")
	}
	return strings.Join(resq, " | "), query, nil
}

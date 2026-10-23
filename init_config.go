package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

func initExisting(update bool) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(defaultConfigFile)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !update {
		return nil, nil, fmt.Errorf("%s already exists; use -update to refresh discovered roots or keep it as is", defaultConfigFile)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s must be a regular file, not a symbolic link or directory", defaultConfigFile)
	}
	raw, err := os.ReadFile(defaultConfigFile)
	return raw, info, err
}

func initConfig(old []byte, roots, dirs, scans []string, setScan bool) ([]byte, error) {
	var doc yaml.Node
	var comments []byte
	if len(old) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(old))
		switch err := decoder.Decode(&doc); {
		case err == io.EOF:
			// A file with only comments has no document to carry them.
			comments = old
		case err != nil:
			return nil, errors.New("existing config is not valid YAML")
		default:
			var extra yaml.Node
			if err := decoder.Decode(&extra); err != io.EOF {
				return nil, errors.New("existing config must contain exactly one YAML document")
			}
			var values map[string]any
			if err := doc.Decode(&values); err != nil {
				return nil, errors.New("existing config must be a mapping with unique keys")
			}
			// Materialize aliases before changing paths. Otherwise an unrelated
			// setting referring to an anchored roots value would change with it.
			var err error
			if doc, err = initWithoutAliases(&doc); err != nil {
				return nil, err
			}
		}
	}
	if len(doc.Content) == 0 || doc.Content[0].Tag == "!!null" {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	body := doc.Content[0]
	if body.Kind != yaml.MappingNode {
		return nil, errors.New("existing config must be a YAML mapping")
	}
	put := func(key string, values []string) error {
		values = slices.Clone(values)
		for i, v := range values {
			// The loader expands ~/ to the home directory.
			if strings.HasPrefix(v, "~/") {
				values[i] = "./" + v
			}
		}
		var value yaml.Node
		if err := value.Encode(values); err != nil {
			return err
		}
		for i := 0; i < len(body.Content); i += 2 {
			if body.Content[i].Value == key {
				old := body.Content[i+1]
				value.Anchor, value.HeadComment, value.LineComment, value.FootComment = old.Anchor, old.HeadComment, old.LineComment, old.FootComment
				body.Content[i+1] = &value
				return nil
			}
		}
		body.Content = append(body.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &value)
		return nil
	}
	if err := put("roots", roots); err != nil {
		return nil, err
	}
	if err := put("state-roots", dirs); err != nil {
		return nil, err
	}
	if setScan {
		if err := put("scan", scans); err != nil {
			return nil, err
		}
	}
	if len(old) == 0 {
		doc.HeadComment = "Terraform roots selected by tforphans init.\nRun tforphans doctor to check providers and access before auditing."
	}
	var output bytes.Buffer
	output.Write(comments)
	if len(comments) > 0 && comments[len(comments)-1] != '\n' {
		output.WriteByte('\n')
	}
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeInitConfig(data, old []byte, previous os.FileInfo) error {
	if previous == nil {
		f, err := os.OpenFile(defaultConfigFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(defaultConfigFile)
		}
		return err
	}
	f, err := os.CreateTemp(".", ".tforphans-init-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(previous.Mode().Perm()); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	current, err := os.Lstat(defaultConfigFile)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(previous, current) {
		return errors.New("config changed during discovery; retry init -update")
	}
	latest, err := os.ReadFile(defaultConfigFile)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, old) {
		return errors.New("config changed during discovery; retry init -update")
	}
	return os.Rename(f.Name(), defaultConfigFile)
}

// Reuse the selected trees when updating; the caller can change them by
// passing DIR explicitly. A custom config without state-roots uses roots.
func initPreviousDirs(raw []byte) ([]string, error) {
	var config map[string]any
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return nil, errors.New("existing config must be a YAML mapping with unique keys")
	}
	for _, key := range []string{"state-roots", "roots"} {
		if value, ok := config[key]; ok {
			dirs, err := configValues(value)
			if err != nil {
				return nil, fmt.Errorf("existing config: %s must list directories", key)
			}
			if len(dirs) == 0 {
				return nil, fmt.Errorf("existing config: %s is empty; pass DIR explicitly", key)
			}
			return dirs, nil
		}
	}
	return nil, nil
}

// initMaxNodes bounds alias expansion: a short file can refer to the same
// anchor many times over.
const initMaxNodes = 1_000_000

type initExpand struct {
	active map[*yaml.Node]bool
	nodes  int
}

func initWithoutAliases(node *yaml.Node) (yaml.Node, error) {
	e := initExpand{active: map[*yaml.Node]bool{}}
	return e.node(node)
}

func (e *initExpand) node(node *yaml.Node) (yaml.Node, error) {
	if e.active[node] {
		return yaml.Node{}, errors.New("existing config contains a recursive alias")
	}
	if e.nodes++; e.nodes > initMaxNodes {
		return yaml.Node{}, errors.New("existing config expands to too many values")
	}
	e.active[node] = true
	defer delete(e.active, node)
	if node.Kind == yaml.AliasNode {
		out, err := e.node(node.Alias)
		if err != nil {
			return out, err
		}
		if node.HeadComment != "" {
			out.HeadComment = node.HeadComment
		}
		if node.LineComment != "" {
			out.LineComment = node.LineComment
		}
		if node.FootComment != "" {
			out.FootComment = node.FootComment
		}
		return out, nil
	}
	out := *node
	out.Anchor = ""
	out.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		c, err := e.node(child)
		if err != nil {
			return out, err
		}
		out.Content[i] = &c
	}
	return out, nil
}

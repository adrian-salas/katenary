package helm

import (
	"errors"
	"io/ioutil"
	"strings"
)

// Values is a representation of the values.yaml file.
type Values map[string]map[string]interface{}

// InlineConfig is made to represent a configMap or a secret
type InlineConfig interface {
	AddEnvFile(filename, name string, values *Values) error
	Metadata() *Metadata
}

// ConfigMap is a configMap file.
type ConfigMap struct {
	*K8sBase `yaml:",inline"`
	Data     map[string]string `yaml:"data"`
}

// NewConfigMap creates a new configMap file.
func NewConfigMap(name string) *ConfigMap {
	base := NewBase()
	base.ApiVersion = "v1"
	base.Kind = "ConfigMap"
	base.Metadata.Name = RELEASE_NAME + "-" + name
	base.Metadata.Labels[K+"/component"] = name
	return &ConfigMap{
		K8sBase: base,
		Data:    make(map[string]string),
	}
}

// Metadata returns the metadata of the secret.
func (c *ConfigMap) Metadata() *Metadata {
	return c.K8sBase.Metadata
}

func (c *ConfigMap) AddEnvFile(file, name string, values *Values) error {
	content, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}

	lines := strings.Split(string(content), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) == 0 {
			continue
		}
		parts := strings.SplitN(l, "=", 2)
		if len(parts) < 2 {
			return errors.New("The environment file " + file + " is not valid")
		}
		c.Data[parts[0]] = parts[1]
	}

	return nil

}

// Secret is a secret for kubernetes
type Secret struct {
	*K8sBase `yaml:",inline"`
	Data     map[string]string `yaml:"data"`
}

// NewSecret creates a new secret file.
func NewSecret(name string) *Secret {
	base := NewBase()
	base.ApiVersion = "v1"
	base.Kind = "Secret"
	base.Metadata.Name = RELEASE_NAME + "-" + name
	base.Metadata.Labels[K+"/component"] = name
	return &Secret{
		K8sBase: base,
		Data:    make(map[string]string),
	}
}

// AddEnvFile adds the content of a file to the secret. It set the value to
// the values content and use "b64enc" filter to encode the content.
func (s *Secret) AddEnvFile(file, name string, values *Values) error {
	content, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}

	tmpValues := *values

	lines := strings.Split(string(content), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) == 0 {
			continue
		}
		parts := strings.SplitN(l, "=", 2)
		if len(parts) < 2 {
			return errors.New("The environment file " + file + " is not valid")
		}

		if _, ok := tmpValues[name]; !ok {
			tmpValues[name] = make(map[string]interface{})
		}
		s.Data[parts[0]] = `{{ .Values.` + name + `.` + parts[0] + ` | b64enc | quote }}`
		tmpValues[name][parts[0]] = parts[1]
	}

	values = &tmpValues
	return nil
}
func (s *Secret) Metadata() *Metadata {
	return s.K8sBase.Metadata
}

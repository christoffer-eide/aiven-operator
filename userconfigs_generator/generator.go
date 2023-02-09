package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dave/jennifer/jen"
	"github.com/google/go-cmp/cmp"
	"github.com/stoewer/go-strcase"
	"golang.org/x/exp/slices"
	"golang.org/x/tools/imports"
	"gopkg.in/yaml.v3"
)

// generate writes to file a service user config for a given serviceList
func generate(dstDir string, serviceTypes []byte, serviceList []string) error {
	// root level object
	var root map[string]*object

	err := yaml.Unmarshal(serviceTypes, &root)
	if err != nil {
		return err
	}

	done := make([]string, 0, len(serviceList))
	for _, k := range serviceList {
		v, ok := root[k]
		if !ok {
			continue
		}

		dirPath := filepath.Join(dstDir, k)
		err = os.MkdirAll(dirPath, os.ModePerm)
		if err != nil {
			return err
		}

		// User config has UserConfig suffix
		b, err := newUserConfigFile(k+"_user_config", v)
		if err != nil {
			log.Println(err)
			continue
		}

		path := filepath.Join(dirPath, k+".go")
		err = os.WriteFile(path, b, 0644)
		if err != nil {
			return err
		}

		done = append(done, k)
	}

	if d := cmp.Diff(serviceList, done); d != "" {
		return fmt.Errorf("not all services are generated: %s", d)
	}
	return nil
}

// newUserConfigFile generates jennifer file from the root object
func newUserConfigFile(name string, obj *object) ([]byte, error) {
	root := toCamelCase(name)
	obj.init(root) // Cascade init from the top
	file := jen.NewFile(strings.ToLower(root))
	file.HeaderComment("Code generated by user config generator. DO NOT EDIT.")

	// Makes kubebuilder generate DeepCopy method for the package
	file.HeaderComment("// +kubebuilder:object:generate=true")
	err := addObject(file, obj)
	if err != nil {
		return nil, err
	}

	// Jenifer won't use imports from code chunks added with Op()
	// Even calling explicit import won't work,
	// cause if module is not used, then it's dropped
	// Calls goimports which fixes missing imports
	b, err := imports.Process("", []byte(file.GoString()), nil)
	if err != nil {
		return nil, err
	}

	return b, nil
}

// objectType json object types
type objectType string

const (
	objectTypeObject  objectType = "object"
	objectTypeArray   objectType = "array"
	objectTypeString  objectType = "string"
	objectTypeBoolean objectType = "boolean"
	objectTypeInteger objectType = "integer"
	objectTypeNumber  objectType = "number"
)

// ObjectInternal internal fields for object
type objectInternal struct {
	jsonName   string // original name from json spec
	structName string // go struct name in CamelCase
	index      int    // field order in object.Properties
}

// object represents OpenApi object
type object struct {
	objectInternal

	// ObjectValidators
	// https://pkg.go.dev/encoding/json#Unmarshal
	// Go returns float64 for JSON numbers
	Enum []*struct {
		Value string `yaml:"value"`
	} `yaml:"enum"`
	Pattern   string   `yaml:"pattern"`
	Minimum   *float64 `yaml:"minimum"`
	Maximum   *float64 `yaml:"maximum"`
	MinItems  *float64 `yaml:"min_items"`
	MaxItems  *float64 `yaml:"max_items"`
	MinLength *float64 `yaml:"min_length"`
	MaxLength *float64 `yaml:"max_length"`

	// OpenAPI Spec
	Type           objectType         `yaml:"-"`
	OrigType       interface{}        `yaml:"type"`
	Format         string             `yaml:"format"`
	Title          string             `yaml:"title"`
	Description    string             `yaml:"description"`
	Properties     map[string]*object `yaml:"properties"`
	ArrayItems     *object            `yaml:"items"`
	RequiredFields []string           `yaml:"required"`
	CreateOnly     bool               `yaml:"create_only"`
	Required       bool               `yaml:"-"`
	// Go doesn't support nullable scalar types, e.g.:
	// type Foo struct {
	//     Foo *bool `json:"foo,omitempty"
	// }
	// To be able to send "false" we use pointer. So "nil" becomes "empty"
	// Then if we need to send "nil", we remove "omitempty"
	//     Foo *bool `json:"foo"
	// Now it is possible to send [null, true, false]
	// But the field becomes required, and it's mandatory to have it manifest
	// We can mark field as "optional" for builder:
	//     // +optional
	//     Foo *bool `json:"foo,omitempty"
	// That means that KubeAPI won't require this field on request.
	// But that would send explicit "nil" to Aiven API.
	// Now you need "default" value to send it instead of "nil", if default is not nil.
	// Adding `+nullable` will fail on API call for the same reason (pointer vs omitempty vs default value)
	// Another reason is that spec is mostly invalid, and nullable fields are not so.
	// So for simplicity this generator doesn't support nullable values.
	Nullable bool `yaml:"-"` // Not really used for now

}

// init initiates object after it gets values from OpenAPI spec
func (o *object) init(name string) {
	o.jsonName = name
	o.structName = toCamelCase(name)

	// Sorts properties so they keep order on each generation
	keys := make([]string, 0, len(o.Properties))
	for k := range o.Properties {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	required := make(map[string]bool, len(o.RequiredFields))
	for _, k := range o.RequiredFields {
		required[k] = true
	}

	for i, k := range keys {
		child := o.Properties[k]
		child.index = i
		child.Required = required[k]
		child.init(k)
	}

	if o.ArrayItems != nil {
		o.ArrayItems.init(name)
		// Slice items always Required, but for GO struct pointers are better
		o.ArrayItems.Required = o.ArrayItems.Type != objectTypeObject
		// Slice items can't be null, if so it is invalid spec
		o.ArrayItems.Nullable = false
	}

	// Types can be list of strings, or a string
	if v, ok := o.OrigType.(string); ok {
		o.Type = objectType(v)
	} else if v, ok := o.OrigType.([]interface{}); ok {
		o.Type = objectType(v[0].(string))
		for _, t := range v {
			switch s := t.(string); s {
			case "null":
				// Enums can't be nullable
				o.Nullable = len(o.Enum) == 0
			case "string":
				o.Type = objectType(s)
			default:
				// Sets if not empty, string is priority
				if o.Type != "" {
					o.Type = objectType(s)
				}
			}
		}
	}
}

// addObject adds object to jen.File
func addObject(file *jen.File, obj *object) error {
	// We need to iterate over fields by index,
	// so new structs and properties are ordered
	// Or we will get diff everytime we generate files
	keyOrder := make([]string, len(obj.Properties))
	for key, child := range obj.Properties {
		keyOrder[child.index] = key
	}

	fields := make([]jen.Code, len(obj.Properties))
	for _, key := range keyOrder {
		child := obj.Properties[key]
		f, err := addField(file, jen.Id(child.structName), child)
		if err != nil {
			return fmt.Errorf("%s: %s", key, err)
		}
		fields[child.index] = f
	}

	// Creates struct and adds fmtComment if available
	s := jen.Type().Id(obj.structName).Struct(fields...)
	if c := fmtComment(obj); c != "" {
		s = jen.Comment(fmtComment(obj)).Line().Add(s)
	}

	// Hacks!
	if obj.jsonName == "ip_filter" {
		file.Line().Op(ipFilterCustomUnmarshal)
	}

	file.Add(s)
	return nil
}

func addField(file *jen.File, s *jen.Statement, obj *object) (*jen.Statement, error) {
	s, err := addFieldType(file, s, obj)
	if err != nil {
		return nil, err
	}

	s = addFieldComments(s, obj)
	s = addFieldTags(s, obj)
	return s.Line(), nil
}

func addFieldType(file *jen.File, s *jen.Statement, obj *object) (*jen.Statement, error) {
	if !obj.Required {
		// Adds to all types, except arrays, which are of pointer type in go
		if obj.Type != objectTypeArray {
			s = s.Op("*")
		}
	}

	switch obj.Type {
	case objectTypeObject:
		err := addObject(file, obj)
		if err != nil {
			return nil, err
		}
		s = s.Id(obj.structName)
	case objectTypeArray:
		return addFieldType(file, s.Index(), obj.ArrayItems)
	case objectTypeString:
		s = s.String()
	case objectTypeBoolean:
		s = s.Bool()
	case objectTypeInteger:
		s = s.Int()
	case objectTypeNumber:
		s = s.Float64()
	default:
		return nil, fmt.Errorf("unknown type %q", obj.Type)
	}
	return s, nil
}

// addFieldTags adds tags for marshal/unmarshal
// with `groups` tag it is possible to mark "create only" fields, like `admin_password`
func addFieldTags(s *jen.Statement, obj *object) *jen.Statement {
	tags := map[string]string{
		"json":   obj.jsonName,
		"groups": "create",
	}

	if !obj.Required {
		tags["json"] += ",omitempty"
	}

	// CreatOnly can't be updated
	if !obj.CreateOnly {
		tags["groups"] += ",update"
	}
	return s.Tag(tags)
}

// addFieldComments add validation markers and doc string
func addFieldComments(s *jen.Statement, obj *object) *jen.Statement {
	c := make([]string, 0)
	// We don't validate floats because of conversion problems (go, json, yaml)
	if obj.Type == objectTypeInteger {
		if obj.Minimum != nil {
			c = append(c, fmt.Sprintf("// +kubebuilder:validation:Minimum=%d", int(*obj.Minimum)))
		}
		if m := objMaximum(obj); m != "" {
			c = append(c, "// +kubebuilder:validation:Maximum="+m)
		}
	}
	if obj.MinLength != nil {
		c = append(c, fmt.Sprintf("// +kubebuilder:validation:MinLength=%d", int(*obj.MinLength)))
	}
	if obj.MaxLength != nil {
		c = append(c, fmt.Sprintf("// +kubebuilder:validation:MaxLength=%d", int(*obj.MaxLength)))
	}
	if obj.MinItems != nil {
		c = append(c, fmt.Sprintf("// +kubebuilder:validation:MinItems=%d", int(*obj.MinItems)))
	}
	if obj.MaxItems != nil {
		c = append(c, fmt.Sprintf("// +kubebuilder:validation:MaxItems=%d", int(*obj.MaxItems)))
	}
	if obj.Pattern != "" {
		_, err := regexp.Compile(obj.Pattern)
		if err != nil {
			log.Printf("can't compile field %q regex `%s`: %s", obj.jsonName, obj.Pattern, err)
		} else {
			c = append(c, fmt.Sprintf("// +kubebuilder:validation:Pattern=`%s`", obj.Pattern))
		}
	}
	if len(obj.Enum) != 0 {
		enum := make([]string, len(obj.Enum))
		for i, s := range obj.Enum {
			enum[i] = safeEnum(s.Value)
		}
		c = append(c, fmt.Sprintf("// +kubebuilder:validation:Enum=%s", strings.Join(enum, ";")))
	}
	if obj.CreateOnly {
		c = append(c, `// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Value is immutable"`)
	}

	doc := fmtComment(obj)
	if doc != "" {
		c = append(c, doc)
	}

	if len(c) != 0 {
		s = jen.Comment(strings.Join(c, "\n")).Line().Add(s)
	}

	return s
}

// fmtComment creates nice comment from object.Title or object.Description (takes the longest string)
func fmtComment(obj *object) string {
	d := ""
	if len(obj.Description) > len(obj.Title) {
		d = obj.Description
	} else {
		d = obj.Title
	}

	if d == "" {
		return d
	}
	// Do not add field/struct name into the comment.
	// Otherwise, generated manifests and docs will have those as a part of the description
	return strings.ReplaceAll("// "+d, "\n", " ")
}

// toCamelCase some fields has dots within, makes cleaner camelCase
func toCamelCase(s string) string {
	return strcase.UpperCamelCase(strings.ReplaceAll(s, ".", "_"))
}

// safeEnumRe operator sdk won't compile enums with special characters
var safeEnumRe = regexp.MustCompile(`[^\w-]`)

// safeEnum returns quoted enum if it contains special characters
func safeEnum(s string) string {
	if safeEnumRe.MatchString(s) {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// objMaximum validates obj maximum
func objMaximum(obj *object) string {
	if obj.Maximum == nil || obj.Type != objectTypeInteger {
		return ""
	}

	// "Maximum" validator is a float64
	// https://github.com/kubernetes-sigs/controller-tools/blob/cb13ac551a0599044e50ed756735a8f438a24631/pkg/crd/markers/validation.go#L128
	// MaxInt (9223372036854775807) becomes float64=9.223372036854776e+18
	// and turing it to int results overflow: -9223372036854775808
	// Validation then fails with "should be less than or equal to -9223372036854775808"
	// Unfortunately type conversion works different for AMD64 and ARM64.
	// So we can't just check conversion here, because we do need this work the very same way on all platforms.
	// We now this happens to big numbers, and the big number we have in API is MaxInt.
	// Skips it, skips overflows.
	m := int(*obj.Maximum)
	if m < 1 || m == math.MaxInt64 {
		return ""
	}

	if obj.Minimum != nil && int(*obj.Minimum) >= m {
		// Maximum must be bigger than minimum
		log.Printf("field %q has minimum >= maximum: %d >= %d", obj.jsonName, int(*obj.Minimum), m)
		return ""
	}
	return fmt.Sprint(m)
}

// ipFilterCustomUnmarshal adds custom UnmarshalJSON that supports both strings and object type
const ipFilterCustomUnmarshal = `
func (ip *IpFilter) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || string(data) == ` + "`" + `""` + "`" + ` {
        return nil
    }
	
	var s string
	err := json.Unmarshal(data, &s)
	if err == nil {
		ip.Network = s
		return nil
	}

	type this struct {
		Network string ` + "`" + `json:"network"` + "`" + `
		Description *string ` + "`" + `json:"description,omitempty" ` + "`" + `
	}
	
	var t *this 
	err = json.Unmarshal(data, &t)
	if err != nil {
		return err
	}
	ip.Network = t.Network
	ip.Description = t.Description
	return nil
}
`

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/go-yaml/yaml"
)

// PluginSpec is
type PluginSpec struct {
	PluginSpecVersion string                          `yaml:"plugin_spec_version"`
	Name              string                          `yaml:"name"`
	Title             string                          `yaml:"title"`
	Description       string                          `yaml:"description"`
	Version           string                          `yaml:"version"`
	Vendor            string                          `yaml:"vendor"`
	Tags              []string                        `yaml:"tags"`
	Icon              string                          `yaml:"icon"`
	Help              string                          `yaml:"help"`
	Connection        map[string]ParamData            `yaml:"connection"`
	RawTypes          map[string]map[string]ParamData `yaml:"types"`
	Types             map[string]TypeData             `yaml:"-"`
	Triggers          map[string]PluginHandlerData    `yaml:"triggers"`
	Actions           map[string]PluginHandlerData    `yaml:"actions"`

	// Things that are not part of the spec, but still important
	PackageRoot string `yaml:"package_root"`
	HTTP        HTTP   `yaml:"http"`

	// Things that are for background help
	TypeMapper        *TypeMapper `yaml:"-"`
	SpecLocation      string      `yaml:"-"`
	ConnectionDataKey string      `yaml:"-"`
}

// ParamData are the details that make up each input/output/trigger param
// Not all data is always filled in, it's context sensitive, which is unfortunate but
// I'm willing to accept it for this since everything else is greatly simplified
type ParamData struct {
	RawName     string        `yaml:"name"`
	Name        string        `yaml:"-"` // This is the joined and camelled name for the param
	Type        string        `yaml:"type"`
	Required    bool          `yaml:"required"`
	Description string        `yaml:"description"`
	Enum        []interface{} `yaml:"enum"`
	Default     interface{}   `yaml:"default"`
	Embed       bool          `yaml:"embed"`
	Nullable    bool          `yaml:"nullable"`

	// Things that are used for background help
	EnumLiteral []EnumData `yaml:"-"`
}

// EnumData is used to parse and write out enums
type EnumData struct {
	Name         string `yaml:"-"`
	LiteralValue string `yaml:"-"`
}

// TypesInternalType is a special hack for the types package. Because in all other places we need to reference X via types.X
// we prefix it ahead of time. But types that use or refer to other types in the types package don't need it.
// This method, used only from the code generators for types, is to make sure types don't use the pacakge name internally in the package
func (p ParamData) TypesInternalType() string {
	if i := strings.Index(p.Type, "types."); i > -1 {
		return strings.Replace(p.Type, "types.", "", -1)
	}
	return p.Type
}

// PluginHandlerData defines the actions or triggers
type PluginHandlerData struct {
	RawName     string `yaml:"name"`
	Name        string `yaml:"-"` // This is the joined and camelled name for the action
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Input       map[string]ParamData
	Output      map[string]ParamData

	// Things that are only used to make parsing templates simpler
	PackageRoot string `yaml:"-"`
}

// TypeData defines the custom types. Much of the data is pulled via a parent-key, so we don't parse much from yaml at all.
// Instead, we post-process populate it for the benefit of the template
type TypeData struct {
	RawName      string               `yaml:"-"`
	Name         string               `yaml:"-"`
	Fields       map[string]ParamData `yaml:"-"`
	SortedFields []ParamData          `yaml:"-"`
}

// HTTP Defines the settings for the plugins http server
type HTTP struct {
	Port         int `yaml:"port"`
	ReadTimeout  int `yaml:"read_timeout"`
	WriteTimeout int `yaml:"write_timeout"`
}

// WHERE I AM - i can load specs
// now, need to generate a hard cope of everything in /templates
// need to shape the plugin spec to the template, or vice versa.
// latter probably faster that this point unless i need to massage
// some datas.
func main() {
	specLoc := flag.String("spec", "", "The path to the spec file")
	packageRoot := flag.String("package", "", "The go package root for this plugin. Ex: github.com/<company_name>/plugins/<plugin_name>")
	flag.Parse()
	if *specLoc == "" {
		log.Fatal("Error, must provide path to spec including name")
	}
	if *packageRoot == "" {
		log.Fatal("Error, must provide a package root for the resulting go package. Ex: github.com/<company_name>/plugins/<plugin_name>")
	}
	data, err := ioutil.ReadFile(*specLoc)
	if err != nil {
		log.Fatalf("error loading spec: %s", err)
	}
	// Chop off the trailing slash, we add it back in as we need it.
	pRoot := *packageRoot
	fmt.Println(pRoot)
	if strings.HasSuffix(*packageRoot, "/") {
		pRoot = pRoot[0 : len(pRoot)-1]
	}
	s := &PluginSpec{
		PackageRoot:  pRoot,
		SpecLocation: *specLoc,
	}
	if err := yaml.Unmarshal(data, s); err != nil {
		log.Fatal(err)
	}
	// Fill in some basic stuff that isn't readily available from the parse, but helps the generation
	postProcessSpec(s)
	// Now, MAKE IT HAPPEN
	if err := generatePlugin(s); err != nil {
		log.Fatal(err)
	}
}

// This is a light weight helper to handle some post-processing boilerplate after parsing a spec
// We need to convert the spec param names into go friendly names, and lookup the proper type
func updateParams(data map[string]ParamData, t *TypeMapper) error {
	for name, param := range data {
		param.RawName = name
		param.Name = UpperCamelCase(name)
		param.Type = t.SpecTypeToGoType(param.Type)
		param.EnumLiteral = make([]EnumData, len(param.Enum))
		for i, e := range param.Enum {
			// So, here's how we're gonna do this: marshal the interface to json
			// this will give us a string representation of the value to write out as a literal
			// then we'll do const X = {{ Literal Value }}
			b, err := json.Marshal(e)
			if err != nil {
				return err
			}
			param.EnumLiteral[i] = EnumData{
				Name:         param.Name + UpperCamelCase(string(b)),
				LiteralValue: string(b),
			}
		}
		data[name] = param // Godbless go for this feature
	}
	return nil
}

// postProcessSpec does some minor post-processing on the spec object to fill a few things in that make
// template generation easier
func postProcessSpec(s *PluginSpec) error {
	// I don't like this dual-dependency shit on typemapper and spec but idgaf right now to bother with it
	t := NewTypeMapper(s)
	s.TypeMapper = t

	// Handle any custom types
	// We'll both populate Types AND update RawTypes so the original source is correct w/r/t the downstream source
	s.Types = make(map[string]TypeData)
	for name, data := range s.RawTypes {
		td := TypeData{}
		td.RawName = name
		td.Name = UpperCamelCase(name)

		if err := updateParams(data, t); err != nil {
			return err
		}

		td.Fields = data
		// Sort them  - currently this is by their embedded status, as embeds must appear uptop in go structs
		td.SortedFields = sortParamData(td.Fields)
		s.RawTypes[name] = data
		s.Types[name] = td
	}

	// fill in the connection names
	// Do this one out long form since we need the special case of building the data key
	for name, param := range s.Connection {
		param.RawName = name
		param.Name = UpperCamelCase(name)
		param.Type = t.SpecTypeToGoType(param.Type)
		s.Connection[name] = param
		if param.Type == "string" {
			if s.ConnectionDataKey != "" {
				s.ConnectionDataKey += " + "
			}
			s.ConnectionDataKey += "c." + param.Name
		}
	}
	if s.ConnectionDataKey == "" {
		// Default it to the literal value of an empty string for now
		// This could be because there were no string params - an issue to solve
		// Or because it doesn't use a connection, which is totally fine
		// TODO if there is no connection to generate, skip the whole connection pkg?
		s.ConnectionDataKey = `""`
	}
	// fill in the trigger names
	for name, action := range s.Actions {
		action.RawName = name // not set in the yaml this way, but set for the benefit of the template
		action.Name = UpperCamelCase(name)
		action.PackageRoot = s.PackageRoot
		// We need to do the same thing for the params too
		if err := updateParams(action.Input, t); err != nil {
			return err
		}
		if err := updateParams(action.Output, t); err != nil {
			return err
		}
		s.Actions[name] = action
	}

	for name, trigger := range s.Triggers {
		trigger.RawName = name // not set in the yaml this way, but set for the benefit of the template
		trigger.Name = UpperCamelCase(name)
		trigger.PackageRoot = s.PackageRoot
		// We need to do the same thing for the params too
		if err := updateParams(trigger.Input, t); err != nil {
			return err
		}
		if err := updateParams(trigger.Output, t); err != nil {
			return err
		}
		s.Triggers[name] = trigger
	}

	if s.HTTP.Port == 0 {
		s.HTTP.Port = 10001
	}

	if s.HTTP.ReadTimeout == 0 {
		s.HTTP.ReadTimeout = 2
	}

	if s.HTTP.WriteTimeout == 0 {
		s.HTTP.WriteTimeout = 2
	}
	return nil
}

func generatePlugin(s *PluginSpec) error {
	// Get GOPATH and then add the plugin root
	p := path.Join(os.Getenv("GOPATH"), "src", s.PackageRoot)
	// if it exists, fail
	if _, err := os.Stat(p); os.IsNotExist(err) {
		// if it doesn't, mkdir-p it
		if err = os.MkdirAll(p, 0700); err != nil {
			log.Fatal("Error when creating plugin package path: " + err.Error())
		}
	}
	if err := generateActions(s); err != nil {
		return err
	}
	if err := generateConnections(s); err != nil {
		return err
	}
	if err := generateTriggers(s); err != nil {
		return err
	}
	if err := generateCmd(s); err != nil {
		return err
	}
	if err := generateHTTPServer(s); err != nil {
		return err
	}
	if err := generateHTTPHandlers(s); err != nil {
		return err
	}
	if err := generateTests(s); err != nil {
		return err
	}
	if err := generateTypes(s); err != nil {
		return err
	}
	if err := generateBuildSupport(s); err != nil {
		return err
	}
	if err := copySpec(s); err != nil {
		return err
	}
	// run goimports before any vendoring
	if err := runGoImports(s); err != nil {
		return err
	}
	if err := vendorPluginDeps(s); err != nil {
		return err
	}
	return nil
}

func doesFileExist(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err) // return !notexisterror because the function is checking if it DOES exist, not if it DOES NOT
}

func runTemplate(templatePath string, outputPath string, data interface{}, skipIfExists bool) error {
	if skipIfExists && doesFileExist(outputPath) {
		return nil // This isn't error, this is just the function deciding to do nothing under these circumstances
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0700); err != nil {
		return err
	}
	b, err := Asset(templatePath)
	if err != nil {
		return err
	}
	tmp := template.New(templatePath)
	t, err := tmp.Parse(string(b))
	if err != nil {
		return err
	}
	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	// now run the template
	if err := t.Execute(f, data); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

func generateActions(s *PluginSpec) error {
	// Now, do one for each action using the action_x template
	pathToActionTemplate := "templates/actions/action_x.template"
	pathToRunTemplate := "templates/actions/action_x_run.template"
	for name, action := range s.Actions {
		// Make the new action.go
		newFilePath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/actions/", name+".go")
		if err := runTemplate(pathToActionTemplate, newFilePath, action, false); err != nil {
			return err
		}
		// Make the new action_run.go
		// action_run is broken out, so that re-generating will skip them if they exist, making it easier for the dev
		newFilePath = path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/actions/", name+"_run.go")
		if err := runTemplate(pathToRunTemplate, newFilePath, action, true); err != nil {
			return err
		}
	}
	return nil
}

func generateConnections(s *PluginSpec) error {
	pathToTemplate := "templates/connection/connection.template"
	newFilePath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/connection/", "connection.go")
	if err := runTemplate(pathToTemplate, newFilePath, s, false); err != nil {
		return err
	}
	// Connect and validate are broken out, so that re-generating will skip them if they exist, making it easier for the dev
	pathToTemplate = "templates/connection/connect.template"
	newFilePath = path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/connection/connect.go")
	if err := runTemplate(pathToTemplate, newFilePath, s, true); err != nil {
		return err
	}
	pathToTemplate = "templates/connection/cache.template"
	newFilePath = path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/connection/cache.go")
	return runTemplate(pathToTemplate, newFilePath, s, false)
}

func generateTriggers(s *PluginSpec) error {
	// Now, do one for each action using the action_x template
	pathToTriggerTemplate := "templates/triggers/trigger_x.template"
	pathToRunTemplate := "templates/triggers/trigger_x_run.template"

	for name, trigger := range s.Triggers {
		// Make the new action.go
		newFilePath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/triggers/", name+".go")
		if err := runTemplate(pathToTriggerTemplate, newFilePath, trigger, false); err != nil {
			return err
		}
		// trigger_run is broken out, so that re-generating will skip them if they exist, making it easier for the dev
		newFilePath = path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/triggers/", name+"_run.go")
		if err := runTemplate(pathToRunTemplate, newFilePath, trigger, true); err != nil {
			return err
		}
	}
	return nil
}

func generateCmd(s *PluginSpec) error {
	pathToTemplate := "templates/cmd/main.template"
	newFilePath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/cmd/", "main.go")
	return runTemplate(pathToTemplate, newFilePath, s, false)
}

func generateHTTPServer(s *PluginSpec) error {
	pathToTemplate := "templates/server/http/server.template"
	newFilePath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/server/http/", "server.go")
	return runTemplate(pathToTemplate, newFilePath, s, false)
}

func generateHTTPHandlers(s *PluginSpec) error {
	// Now, do one for each action using the action_x template
	pathToTemplate := "templates/server/http/handler_x.template"
	for name, action := range s.Actions {
		// Make the new action.go
		newFilePath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/server/http/", name+".go")
		if err := runTemplate(pathToTemplate, newFilePath, action, false); err != nil {
			return err
		}
	}
	return nil
}

func generateTests(s *PluginSpec) error {
	return nil
}

func generateTypes(s *PluginSpec) error {
	// Now, do one for each action using the type_x template
	pathToTemplate := "templates/types/type_x.template"
	for name, t := range s.Types {
		// Make the new action.go
		newFilePath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/types/", name+".go")
		if err := runTemplate(pathToTemplate, newFilePath, t, false); err != nil {
			return err
		}
	}
	return nil
}

func generateBuildSupport(s *PluginSpec) error {
	// Docker
	pathToTemplate := "templates/Dockerfile.template"
	newFilePath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "Dockerfile")
	if err := runTemplate(pathToTemplate, newFilePath, s, false); err != nil {
		return err
	}

	// Make
	pathToTemplate = "templates/Makefile.template"
	newFilePath = path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "Makefile")
	if err := runTemplate(pathToTemplate, newFilePath, s, false); err != nil {
		return err
	}

	// Make the vendor directory for them, add a .gitkeep just incase
	if err := os.MkdirAll(path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/vendor/"), 0700); err != nil {
		return err
	}
	if err := ioutil.WriteFile(path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "/vendor/.gitkeep"), make([]byte, 0), 0644); err != nil {
		return err
	}
	return nil
}

func copySpec(s *PluginSpec) error {
	// Read all content of src to data
	data, err := ioutil.ReadFile(s.SpecLocation)
	if err != nil {
		return err
	}
	// Write data to dst
	return ioutil.WriteFile(path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot, "plugin.spec.yaml"), data, 0644)
}

func runGoImports(s *PluginSpec) error {
	// TODO add tests?
	// TODO pivot to using goimports, which expects whole files, not packages :/
	searchDir := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot)

	fileList := []string{}
	err := filepath.Walk(searchDir, func(path string, f os.FileInfo, err error) error {
		if !strings.Contains(path, "/vendor/") && strings.HasSuffix(path, ".go") {
			fileList = append(fileList, path)
		}
		return nil
	})

	if err != nil {
		return err
	}

	for _, p := range fileList {
		cmd := exec.Command("goimports", "-w", "-srcdir", s.PackageRoot, p)
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("Error while running go imports on %s: %s", p, string(b))
		}
		if err := fixGoImportsNotKnowingHowToLookInLocalVendorFirst(s, p); err != nil {
			return err
		}
	}
	return nil
}

func vendorPluginDeps(s *PluginSpec) error {
	rootPath := path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot)
	cmd := exec.Command("dep", "init")
	if doesFileExist(path.Join(rootPath, "manifest.json")) {
		cmd = exec.Command("dep", "ensure")
	}
	cmd.Dir = path.Join(os.Getenv("GOPATH"), "/src/", s.PackageRoot)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Error while running go dep on %s: %s - %s", cmd.Dir, string(b), err.Error())
	}
	return nil
}

func fixGoImportsNotKnowingHowToLookInLocalVendorFirst(s *PluginSpec, path string) error {
	old := "github.com/komand/komand/plugins/v1/types"
	new := s.PackageRoot + "/types"
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil
	}
	return ioutil.WriteFile(path, []byte(strings.Replace(string(b), old, new, -1)), 0)
}

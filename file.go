package mycli

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

// Section represents a labeled section of an option file. Option values that
// precede any named section are still associated with a Section object, but
// with a Name of "".
type Section struct {
	Name   string
	Values map[string]string
}

// File represents a form of ini-style option file. Lines can contain
// [sections], option=value, option without value (usually for bools), or
// comments.
type File struct {
	Dir                  string
	Name                 string
	IgnoreUnknownOptions bool
	sections             []*Section
	sectionIndex         map[string]*Section
	read                 bool
	parsed               bool
	contents             string
	selected             []string
}

// NewFile returns a value representing an option file. The arg(s) will be
// joined to create a single path, so it does not matter if the path is provided
// in a way that separates the dir from the base filename or not.
func NewFile(paths ...string) *File {
	pathAndName := path.Join(paths...)
	cleanPath, err := filepath.Abs(filepath.Clean(pathAndName))
	if err == nil {
		pathAndName = cleanPath
	}

	return &File{
		Dir:          path.Dir(pathAndName),
		Name:         path.Base(pathAndName),
		sections:     make([]*Section, 0),
		sectionIndex: make(map[string]*Section),
	}
}

// Exists returns true if the file exists and is visible to the current user.
func (f *File) Exists() bool {
	_, err := os.Stat(f.Path())
	return (err == nil)
}

// Path returns the file's full absolute path with filename.
func (f *File) Path() string {
	return path.Join(f.Dir, f.Name)
}

// Write writes out the file's contents to disk.
func (f *File) Write(overwrite bool) error {
	lines := make([]string, 0)
	for n, section := range f.sections {
		if section.Name != "" {
			lines = append(lines, fmt.Sprintf("[%s]", section.Name))
		}
		for k, v := range section.Values {
			lines = append(lines, fmt.Sprintf("%s=%s", k, v))
		}
		if n < len(f.sections)-1 {
			lines = append(lines, "")
		}
	}

	if len(lines) == 0 {
		log.Printf("Skipping write to %s due to empty configuration", f.Path())
		return nil
	}
	f.contents = fmt.Sprintf("%s\n", strings.Join(lines, "\n"))
	f.read = true
	f.parsed = true

	flag := os.O_WRONLY | os.O_CREATE
	if overwrite {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_EXCL
	}
	osFile, err := os.OpenFile(f.Path(), flag, 0666)
	if err != nil {
		return err
	}
	n, err := osFile.Write([]byte(f.contents))
	if err == nil && n < len(f.contents) {
		err = io.ErrShortWrite
	}
	if err1 := osFile.Close(); err == nil {
		err = err1
	}
	return err
}

// Read loads the contents of the option file, but does not parse it.
func (f *File) Read() error {
	file, err := os.Open(f.Path())
	if err != nil {
		return err
	}
	defer file.Close()
	bytes, err := ioutil.ReadAll(file)
	if err != nil {
		return err
	}
	f.contents = string(bytes)
	f.read = true
	return nil
}

// Parse parses the file contents into a series of Sections. A Config object
// must be supplied so that the list of valid Options is known.
func (f *File) Parse(cfg *Config) error {
	if !f.read {
		if err := f.Read(); err != nil {
			return err
		}
	}

	section := &Section{
		Name:   "",
		Values: make(map[string]string),
	}
	f.sections = append(f.sections, section)
	f.sectionIndex[""] = section

	var lineNumber int
	scanner := bufio.NewScanner(strings.NewReader(f.contents))
	for scanner.Scan() {
		line := scanner.Text()
		lineNumber++
		line = strings.TrimLeftFunc(line, unicode.IsSpace)
		if line == "" {
			continue
		}
		if line[0] == '[' {
			name := line[1 : len(line)-1]
			section = f.getOrCreateSection(name)
			continue
		}

		tokens := strings.SplitN(line, "#", 2)
		key, value, loose := NormalizeOptionToken(tokens[0])
		source := fmt.Sprintf("%s line %d", f.Path(), lineNumber)
		opt := cfg.FindOption(key)
		if opt == nil {
			if loose || f.IgnoreUnknownOptions {
				continue
			} else {
				return OptionNotDefinedError{key, source}
			}
		}
		if value == "" {
			if opt.RequireValue {
				return OptionMissingValueError{opt.Name, source}
			} else if opt.Type == OptionTypeBool {
				// Option without value indicates option is being enabled if boolean
				value = "1"
			}
		}

		section.Values[key] = value
	}

	f.parsed = true
	f.selected = []string{""}
	return scanner.Err()
}

// UseSection changes which section(s) of the file are used when calling
// OptionValue. If multiple section names are supplied, multiple sections will
// be checked by OptionValue, with sections listed first taking precedence over
// subsequent ones.
// Note that the default nameless section "" (i.e. lines at the top of the file
// prior to a section header) is automatically appended to the end of the list.
// So this section is always checked, at lowest priority, need not be
// passed to this function.
func (f *File) UseSection(names ...string) error {
	notFound := make([]string, 0)
	already := make(map[string]bool, len(names))
	f.selected = make([]string, 0, len(names)+1)

	for _, name := range names {
		if already[name] {
			continue
		}
		already[name] = true
		if _, ok := f.sectionIndex[name]; ok {
			f.selected = append(f.selected, name)
		} else {
			notFound = append(notFound, name)
		}
	}
	if !already[""] {
		f.selected = append(names, "")
	}

	if len(notFound) == 0 {
		return nil
	}
	return fmt.Errorf("File %s missing section: %s", f.Path(), strings.Join(notFound, ", "))
}

// OptionValue returns the value for the requested option from the option file.
// Only the previously-selected section(s) of the file will be used, or the
// default section "" if no section has been selected via UseSection.
// Panics if the file has not yet been parsed, as this would indicate a bug.
// This is satisfies the OptionValuer interface, allowing Files to be used as
// an option source in Config.
func (f *File) OptionValue(optionName string) (string, bool) {
	if !f.parsed {
		panic(fmt.Errorf("Call to OptionValue(\"%s\") on unparsed file %s", optionName, f.Path()))
	}
	for _, sectionName := range f.selected {
		section := f.sectionIndex[sectionName]
		if section == nil {
			continue
		}
		if value, ok := section.Values[optionName]; ok {
			return value, true
		}
	}
	return "", false
}

// SetOptionValue sets an option value in the named section. This is not
// persisted to the file until Write is called on the File.
func (f *File) SetOptionValue(sectionName, optionName, value string) {
	section := f.getOrCreateSection(sectionName)
	section.Values[optionName] = value
}

func (f *File) getOrCreateSection(name string) *Section {
	if s, exists := f.sectionIndex[name]; exists {
		return s
	}
	s := &Section{
		Name:   name,
		Values: make(map[string]string),
	}
	f.sections = append(f.sections, s)
	f.sectionIndex[name] = s
	return s
}

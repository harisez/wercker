package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const homePrefix = "~/"

// expandHomePath will expand ~/ in p to home.
func expandHomePath(p string, home string) string {
	if strings.HasPrefix(p, homePrefix) {
		return path.Join(home, strings.TrimPrefix(p, homePrefix))
	}

	return p
}

// exists is like python's os.path.exists and too many lines in Go
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// fetchTarball tries to fetch a tarball
// For now this is pretty naive and useless, but we are doing it in a couple
// places and this is a fine stub to expand upon.
func fetchTarball(url string) (*http.Response, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return resp, fmt.Errorf("Bad status code fetching tarball: %s", url)
	}

	return resp, nil
}

// Write the contents up a single file to dst
func untarOne(name string, dst io.Writer, src io.ReadCloser) error {
	// ungzipped, err := gzip.NewReader(src)
	// if err != nil {
	//   return err
	// }
	tarball := tar.NewReader(src)
	defer src.Close()
	// defer tarball.Close()

	for {
		hdr, err := tarball.Next()
		if err == io.EOF {
			// finished the tar
			break
		}
		if err != nil {
			return err
		}

		if hdr.Name != name {
			continue
		}

		// We found the file we care about
		_, err = io.Copy(dst, tarball)
		break
	}
	return nil
}

// untargzip tries to untar-gzip stuff to a path
func untargzip(path string, r io.Reader) error {
	ungzipped, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tarball := tar.NewReader(ungzipped)

	defer ungzipped.Close()

	// We have to treat things differently for git-archives
	isGitArchive := false

	// Alright, things seem in order, let's make the base directory
	os.MkdirAll(path, 0755)
	for {
		hdr, err := tarball.Next()
		if err == io.EOF {
			// finished the tar
			break
		}
		if err != nil {
			return err
		}
		// Skip the base dir
		if hdr.Name == "./" {
			continue
		}

		// If this was made with git-archive it will be in kinda an ugly
		// format, but we can identify it by the pax_global_header "file"
		name := hdr.Name
		if name == "pax_global_header" {
			isGitArchive = true
			continue
		}

		// It will also contain an extra subdir that we will automatically strip
		if isGitArchive {
			parts := strings.Split(name, "/")
			name = strings.Join(parts[1:], "/")
		}

		fpath := filepath.Join(path, name)
		if hdr.FileInfo().IsDir() {
			err = os.MkdirAll(fpath, 0755)
			if err != nil {
				return err
			}
			continue
		}
		file, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE, hdr.FileInfo().Mode())
		defer file.Close()
		if err != nil {
			return err
		}
		_, err = io.Copy(file, tarball)
		if err != nil {
			return err
		}
		file.Close()
	}
	return nil
}

// Finisher is a helper class for running something either right away or
// at `defer` time.
type Finisher struct {
	callback   func(interface{})
	isFinished bool
}

// NewFinisher returns a new Finisher with a callback.
func NewFinisher(callback func(interface{})) *Finisher {
	return &Finisher{callback: callback, isFinished: false}
}

// Finish executes the callback if it hasn't been run yet.
func (f *Finisher) Finish(result interface{}) {
	if f.isFinished {
		return
	}
	f.isFinished = true
	f.callback(result)
}

// Retrieving user input utility functions

func askForConfirmation() bool {
	var response string
	_, err := fmt.Scanln(&response)
	if err != nil {
		rootLogger.WithField("Logger", "Util").Fatal(err)
	}
	response = strings.ToLower(response)
	if strings.HasPrefix(response, "y") {
		return true
	} else if strings.HasPrefix(response, "n") {
		return false
	} else {
		println("Please type yes or no and then press enter:")
		return askForConfirmation()
	}
}

// Counter is a simple struct
type Counter struct {
	Current int
	l       sync.Mutex
}

// Increment will return current and than increment c.Current.
func (c *Counter) Increment() int {
	c.l.Lock()
	defer c.l.Unlock()

	current := c.Current
	c.Current = current + 1

	return current
}

// ContainsString checks if the array items contains the string target.
// TODO(bvdberg): write units tests
func ContainsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// queryString converts a struct to a map. It looks for items with a qs tag.
// This code was taken from the fsouza/go-dockerclient, and then slightly
// modified. See: https://github.com/fsouza/go-dockerclient/blob/5fa67ac8b52afe9430a490391a639085e9357e1e/client.go#L535
func queryString(opts interface{}) map[string]interface{} {
	items := map[string]interface{}{}
	if opts == nil {
		return items
	}
	value := reflect.ValueOf(opts)
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return items
	}
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if field.PkgPath != "" {
			continue
		}
		key := field.Tag.Get("qs")
		if key == "" {
			key = strings.ToLower(field.Name)
		} else if key == "-" {
			continue
		}
		v := value.Field(i)
		switch v.Kind() {
		case reflect.Bool:
			if v.Bool() {
				items[key] = "1"
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if v.Int() > 0 {
				items[key] = strconv.FormatInt(v.Int(), 10)
			}
		case reflect.Float32, reflect.Float64:
			if v.Float() > 0 {
				items[key] = strconv.FormatFloat(v.Float(), 'f', -1, 64)
			}
		case reflect.String:
			if v.String() != "" {
				items[key] = v.String()
			}
		case reflect.Ptr:
			if !v.IsNil() {
				if b, err := json.Marshal(v.Interface()); err == nil {
					items[key] = string(b)
				}
			}
		case reflect.Map:
			if len(v.MapKeys()) > 0 {
				if b, err := json.Marshal(v.Interface()); err == nil {
					items[key] = string(b)
				}
			}
		}
	}
	return items
}

var buildRegex = regexp.MustCompile("^[0-9a-fA-F]{24}$")

// IsBuildID checks if input is a BuildID. BuildID is defined as a 24 character
// hex string.
func IsBuildID(input string) bool {
	return buildRegex.Match([]byte(input))
}

// ParseApplicationID parses input and returns the username and application
// name. A valid application ID is two strings separated by a /.
func ParseApplicationID(input string) (username, name string, err error) {
	split := strings.Split(input, "/")
	if len(split) == 2 {
		return split[0], split[1], nil
	}
	return "", "", errors.New("Unable to parse applicationID")
}

// CounterReader is a io.Reader which wraps a other io.Reader and stores the
// bytes reader from it.
type CounterReader struct {
	r io.Reader
	c int64
}

// NewCounterReader creates a new CounterReader.
func NewCounterReader(r io.Reader) *CounterReader {
	return &CounterReader{r: r}
}

// Read proxy's the request to r, and stores the bytes read as reported by r.
func (c *CounterReader) Read(p []byte) (int, error) {
	read, err := c.r.Read(p)

	c.c += int64(read)

	return read, err
}

// Count returns the bytes read from r.
func (c *CounterReader) Count() int64 {
	return c.c
}

// MinInt finds the smallest int in input and return that value. If no input is
// given, it will return 0.
func MinInt(input ...int) int {
	if len(input) == 0 {
		return 0
	}

	min := input[0]
	for _, in := range input {
		if in < min {
			min = in
		}
	}

	return min
}

// MaxInt finds the biggest int in input and return that value. If no input is
// given, it will return 0.
func MaxInt(input ...int) int {
	if len(input) == 0 {
		return 0
	}

	max := input[0]
	for _, in := range input {
		if in > max {
			max = in
		}
	}
	return max
}

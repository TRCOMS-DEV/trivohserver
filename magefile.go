// +build mage

package main

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"go/build"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/target"

	"github.com/livekit/livekit-server/version"
)

const (
	goChecksumFile = ".checksumgo"
	imageName      = "livekit/livekit-server"
)

// Default target to run when none is specified
// If not set, running mage will list available targets
var Default = Build
var checksummer = NewChecksummer(".", goChecksumFile, ".go", ".mod")

func init() {
	checksummer.IgnoredPaths = []string{
		"pkg/service/wire_gen.go",
		"pkg/rtc/types/typesfakes",
	}
}

// explicitly reinstall all deps
func Deps() error {
	return installTools(true)
}

type modInfo struct {
	Path      string
	Version   string
	Time      time.Time
	Dir       string
	GoMod     string
	GoVersion string
}

// regenerate protobuf
func Proto() error {
	cmd := exec.Command("go", "list", "-m", "-json", "github.com/livekit/protocol")
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	info := modInfo{}
	if err = json.Unmarshal(out, &info); err != nil {
		return err
	}
	protoDir := info.Dir
	updated, err := target.Path("proto/livekit_models.pb.go",
		protoDir+"/livekit_models.proto",
		protoDir+"/livekit_room.proto",
		protoDir+"/livekit_rtc.proto",
		protoDir+"/livekit_internal.proto",
	)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}

	fmt.Println("generating protobuf")
	target := "proto"
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}

	protoc, err := getToolPath("protoc")
	if err != nil {
		return err
	}
	protocGoPath, err := getToolPath("protoc-gen-go")
	if err != nil {
		return err
	}
	twirpPath, err := getToolPath("protoc-gen-twirp")
	if err != nil {
		return err
	}

	// generate model and room
	cmd = exec.Command(protoc,
		"--go_out", target,
		"--twirp_out", target,
		"--go_opt=paths=source_relative",
		"--twirp_opt=paths=source_relative",
		"--plugin=go="+protocGoPath,
		"--plugin=twirp="+twirpPath,
		"-I="+protoDir,
		protoDir+"/livekit_room.proto",
	)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	// generate rtc
	cmd = exec.Command(protoc,
		"--go_out", target,
		"--go_opt=paths=source_relative",
		"--plugin=go="+protocGoPath,
		"-I="+protoDir,
		protoDir+"/livekit_rtc.proto",
		protoDir+"/livekit_internal.proto",
		protoDir+"/livekit_models.proto",
	)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

// builds LiveKit server
func Build() error {
	mg.Deps(Proto, generateWire)
	if !checksummer.IsChanged() {
		fmt.Println("up to date")
		return nil
	}

	fmt.Println("building...")
	if err := os.MkdirAll("bin", 0755); err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", "../../bin/livekit-server")
	cmd.Dir = "cmd/server"
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	checksummer.WriteChecksum()
	return nil
}

// builds docker image for LiveKit server
func Docker() error {
	mg.Deps(Proto, generateWire)
	cmd := exec.Command("docker", "build", ".", "-t", fmt.Sprintf("%s:v%s", imageName, version.Version))
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func PublishDocker() error {
	mg.Deps(Docker)

	versionImg := fmt.Sprintf("%s:v%s", imageName, version.Version)
	cmd := exec.Command("docker", "push", versionImg)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	idx := strings.LastIndex(version.Version, ".")
	minorImg := fmt.Sprintf("%s:v%s", imageName, version.Version[:idx])
	cmd = exec.Command("docker", "tag", versionImg, minorImg)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}
	cmd = exec.Command("docker", "push", minorImg)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	latestImg := fmt.Sprintf("%s:latest", imageName)
	cmd = exec.Command("docker", "tag", versionImg, latestImg)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}
	cmd = exec.Command("docker", "push", latestImg)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// run unit tests, skipping integration
func Test() error {
	mg.Deps(Proto)
	cmd := exec.Command("go", "test", "-short", "./...")
	connectStd(cmd)
	return cmd.Run()
}

// run all tests including integration
func TestAll() error {
	mg.Deps(Proto)
	cmd := exec.Command("go", "test", "./...", "-count=1", "-timeout=1m")
	connectStd(cmd)
	return cmd.Run()
}

// cleans up builds
func Clean() {
	fmt.Println("cleaning...")
	os.RemoveAll("bin")
	os.Remove(goChecksumFile)
}

// regenerate code
func Generate() error {
	mg.Deps(installDeps)

	fmt.Println("generating...")

	cmd := exec.Command("go", "generate", "./...")
	connectStd(cmd)
	return cmd.Run()
}

// code generation for wiring
func generateWire() error {
	mg.Deps(installDeps, Proto)
	if !checksummer.IsChanged() {
		return nil
	}

	fmt.Println("wiring...")

	cmd := exec.Command("go", "generate", "./cmd/...")
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	wire, err := getToolPath("wire")
	if err != nil {
		return err
	}
	cmd = exec.Command(wire)
	cmd.Dir = "pkg/service"
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

// implicitly install deps
func installDeps() error {
	return installTools(false)
}

func installTools(force bool) error {
	if _, err := getToolPath("protoc"); err != nil {
		return fmt.Errorf("protoc is required but is not found")
	}

	tools := []string{
		"google.golang.org/protobuf/cmd/protoc-gen-go",
		"github.com/twitchtv/twirp/protoc-gen-twirp",
		"github.com/google/wire/cmd/wire",
	}
	for _, t := range tools {
		if err := installTool(t, force); err != nil {
			return err
		}
	}
	return nil
}

func installTool(url string, force bool) error {
	name := filepath.Base(url)
	if !force {
		_, err := getToolPath(name)
		if err == nil {
			// already installed
			return nil
		}
	}

	fmt.Printf("installing %s\n", name)
	cmd := exec.Command("go", "get", "-u", url)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	// check
	_, err := getToolPath(name)
	return err
}

// helpers

func getToolPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	// check under gopath
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = build.Default.GOPATH
	}
	p := filepath.Join(gopath, "bin", name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

func connectStd(cmd *exec.Cmd) {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
}

// A helper checksum library that generates a fast, non-portable checksum over a directory of files
// it's designed as a quick way to bypass
type Checksummer struct {
	dir          string
	file         string
	checksum     string
	allExts      bool
	extMap       map[string]bool
	IgnoredPaths []string
}

func NewChecksummer(dir string, checksumfile string, exts ...string) *Checksummer {
	c := &Checksummer{
		dir:    dir,
		file:   checksumfile,
		extMap: make(map[string]bool),
	}
	if len(exts) == 0 {
		c.allExts = true
	} else {
		for _, ext := range exts {
			c.extMap[ext] = true
		}
	}

	return c
}

func (c *Checksummer) IsChanged() bool {
	// default changed
	if err := c.computeChecksum(); err != nil {
		log.Println("could not compute checksum", err)
		return true
	}
	// read
	existing, err := c.ReadChecksum()
	if err != nil {
		// may not be there
		return true
	}

	return existing != c.checksum
}

func (c *Checksummer) ReadChecksum() (string, error) {
	b, err := ioutil.ReadFile(filepath.Join(c.dir, c.file))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (c *Checksummer) WriteChecksum() error {
	if err := c.computeChecksum(); err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(c.dir, c.file), []byte(c.checksum), 0644)
}

func (c *Checksummer) computeChecksum() error {
	if c.checksum != "" {
		return nil
	}

	entries := make([]string, 0)
	ignoredMap := make(map[string]bool)
	for _, f := range c.IgnoredPaths {
		ignoredMap[f] = true
	}
	err := filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if path == c.dir {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") || ignoredMap[path] {
			if info.IsDir() {
				return filepath.SkipDir
			} else {
				return nil
			}
		}
		if info.IsDir() {
			entries = append(entries, fmt.Sprintf("%s %d", path, info.ModTime().Unix()))
		} else if c.allExts || c.extMap[filepath.Ext(info.Name())] {
			entries = append(entries, fmt.Sprintf("%s %d %d", path, info.Size(), info.ModTime().Unix()))
		}
		return nil
	})
	if err != nil {
		return err
	}

	sort.Strings(entries)

	h := sha1.New()
	for _, e := range entries {
		h.Write([]byte(e))
	}
	c.checksum = fmt.Sprintf("%x", h.Sum(nil))

	return nil
}

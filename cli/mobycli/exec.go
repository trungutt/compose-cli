/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package mobycli

import (
	"bufio"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	apicontext "github.com/docker/compose-cli/api/context"
	"github.com/docker/compose-cli/api/context/store"
	"github.com/docker/compose-cli/cli/metrics"
	"github.com/docker/compose-cli/cli/mobycli/resolvepath"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/google/shlex"
	"github.com/spf13/cobra"
)

var delegatedContextTypes = []string{store.DefaultContextType}

// ComDockerCli name of the classic cli binary
var ComDockerCli = "com.docker.cli"

func init() {
	if runtime.GOOS == "windows" {
		ComDockerCli += ".exe"
	}
}

// ExecIfDefaultCtxType delegates to com.docker.cli if on moby context
func ExecIfDefaultCtxType(ctx context.Context, root *cobra.Command) {
	currentContext := apicontext.Current()

	s := store.Instance()

	currentCtx, err := s.Get(currentContext)
	// Only run original docker command if the current context is not ours.
	if err != nil || mustDelegateToMoby(currentCtx.Type()) {
		Exec(root)
	}
}

func mustDelegateToMoby(ctxType string) bool {
	for _, ctype := range delegatedContextTypes {
		if ctxType == ctype {
			return true
		}
	}
	return false
}

// Exec delegates to com.docker.cli if on moby context
func Exec(root *cobra.Command) {
	metricsClient := metrics.NewClient()
	metricsClient.WithCliVersionFunc(func() string {
		return CliVersion()
	})
	childExit := make(chan bool)
	err := RunDocker(childExit, os.Args[1:]...)
	childExit <- true
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			exitCode := exiterr.ExitCode()
			metricsClient.Track(store.DefaultContextType, os.Args[1:], compose.ByExitCode(exitCode).MetricsStatus)
			os.Exit(exitCode)
		}
		metricsClient.Track(store.DefaultContextType, os.Args[1:], compose.FailureStatus)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	commandArgs := os.Args[1:]
	command := metrics.GetCommand(commandArgs)
	if command == "login" && !metrics.HasQuietFlag(commandArgs) {
		displayPATSuggestMsg(commandArgs)
	}
	metricsClient.Track(store.DefaultContextType, os.Args[1:], compose.SuccessStatus)

	os.Exit(0)
}

func enhance(prefix, str, id string) string {
	OSC := "\u001B]"
	BEL := "\u0007"
	SEP := ";"
	deeplink := "docker-desktop://dashboard/" + prefix + "?id=" + id
	enhanced := []string{OSC, "8", SEP, SEP, deeplink, BEL, str, OSC, "8", SEP, SEP, BEL}
	return strings.Join(enhanced[:], "")
}

type Container struct {
	ShortId string
	ID      string
	Names   string
}

func getContainers() []Container {
	var cs []Container
	cmd := exec.Command(comDockerCli(), "ps", "--all", "--no-trunc", "--format", "{\"ID\":\"{{ .ID }}\", \"Names\":\"{{ .Names }}\"}")
	out, err := cmd.Output()
	if err != nil {
		return []Container{}
	}
	csStr := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, cStr := range csStr {
		var c Container
		err := json.Unmarshal([]byte(cStr), &c)
		if err != nil {
			continue
		}
		c.ShortId = c.ID[:12]
		cs = append(cs, c)
	}

	return cs
}

type Image struct {
	ShortId     string
	ID          string
	Tag         string
	Respository string
}

func getImages() []Image {
	var imgs []Image
	cmd := exec.Command(comDockerCli(), "image", "ls", "--no-trunc", "--format", "{\"ID\":\"{{ .ID }}\", \"Tag\":\"{{ .Tag }}\", \"Respository\":\"{{ .Repository }}\"}")
	out, err := cmd.Output()
	if err != nil {
		return []Image{}
	}
	imgsStr := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, imgStr := range imgsStr {
		var img Image
		err := json.Unmarshal([]byte(imgStr), &img)
		if err != nil {
			continue
		}
		img.ShortId = strings.TrimPrefix(img.ID, "sha256:")[:12]
		imgs = append(imgs, img)
	}

	return imgs
}

type Volume struct {
	Name string
}

func getVolumes() []Volume {
	var vols []Volume
	cmd := exec.Command(comDockerCli(), "volume", "ls", "--format", "{\"Name\":\"{{ .Name }}\"}")
	out, err := cmd.Output()
	if err != nil {
		return []Volume{}
	}
	volsStr := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, volStr := range volsStr {
		var vol Volume
		err := json.Unmarshal([]byte(volStr), &vol)
		if err != nil {
			continue
		}
		vols = append(vols, vol)
	}

	return vols
}

// RunDocker runs a docker command, and forward signals to the shellout command (stops listening to signals when an event is sent to childExit)
func RunDocker(childExit chan bool, args ...string) error {
	cmd := exec.Command(comDockerCli(), args...)
	cons := getContainers()
	vols := getVolumes()
	imgs := getImages()
	
	cmd.Stdin = os.Stdin
	// cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	signals := make(chan os.Signal, 1)
	signal.Notify(signals) // catch all signals
	go func() {
		for {
			select {
			case sig := <-signals:
				if cmd.Process == nil {
					continue // can happen if receiving signal before the process is actually started
				}
				// In go1.14+, the go runtime issues SIGURG as an interrupt to
				// support preemptable system calls on Linux. Since we can't
				// forward that along we'll check that here.
				if isRuntimeSig(sig) {
					continue
				}
				_ = cmd.Process.Signal(sig)
			case <-childExit:
				return
			}
		}
	}()

	r, w, _ := os.Pipe()
	cmd.Stdout = w

	done := make(chan struct{})
	// copy the output in a separate goroutine so printing can't block indefinitely
	go func() {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			if args[0] == "run" {
				cons = getContainers()
				vols = getVolumes()
				imgs = getImages()
			}

			line := scanner.Text()
			for _, c := range cons {
				cRegex := regexp.MustCompile("(" + c.ShortId + "[a-z0-9]*" + ")")
				line = cRegex.ReplaceAllString(line, enhance("containers", "$1", c.ID))

				cRegex = regexp.MustCompile("(" + c.Names + ")")
				line = cRegex.ReplaceAllString(line, enhance("containers", "$1", c.ID))
			}

			for _, v := range vols {
				cRegex := regexp.MustCompile("(" + v.Name + ")")
				line = cRegex.ReplaceAllString(line, enhance("volumes", "$1", v.Name))
			}

			for _, img := range imgs {
				if strings.Contains(line, img.Respository) {
					cRegex := regexp.MustCompile("(" + img.Respository + ")")
					if strings.Contains(line, img.Tag) {
						line = cRegex.ReplaceAllString(line, enhance("images", "$1", img.ID+"-"+img.Tag))
					} else {
						line = cRegex.ReplaceAllString(line, enhance("images", "$1", img.ID+"-latest"))
					}
				}

				// cRegex := regexp.MustCompile("(" + img.ShortId + "[a-z0-9]*" + ")")
				// line = cRegex.ReplaceAllString(line, enhance("images", "$1", img.ID+"-"+img.Tag))
			}

			fmt.Fprintln(os.Stdout, line)
		}
		done <- struct{}{}
	}()

	// execute the command
	if err := cmd.Run(); err != nil {
		return err
	}

	w.Close()
	<-done

	return nil
}

func comDockerCli() string {
	if v := os.Getenv("DOCKER_COM_DOCKER_CLI"); v != "" {
		return v
	}

	execBinary := findBinary(ComDockerCli)
	if execBinary == "" {
		var err error
		execBinary, err = resolvepath.LookPath(ComDockerCli)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintln(os.Stderr, "Current PATH : "+os.Getenv("PATH"))
			os.Exit(1)
		}
	}

	return execBinary
}

func findBinary(filename string) string {
	currentBinaryPath, err := os.Executable()
	if err != nil {
		return ""
	}
	currentBinaryPath, err = filepath.EvalSymlinks(currentBinaryPath)
	if err != nil {
		return ""
	}
	binaryPath := filepath.Join(filepath.Dir(currentBinaryPath), filename)
	if _, err := os.Stat(binaryPath); err != nil {
		return ""
	}
	return binaryPath
}

// IsDefaultContextCommand checks if the command exists in the classic cli (issues a shellout --help)
func IsDefaultContextCommand(dockerCommand string) bool {
	cmd := exec.Command(comDockerCli(), dockerCommand, "--help")
	b, e := cmd.CombinedOutput()
	if e != nil {
		fmt.Println(e)
	}
	return regexp.MustCompile("Usage:\\s*docker\\s*" + dockerCommand).Match(b)
}

// CliVersion returns the docker cli version
func CliVersion() string {
	info, err := buildinfo.ReadFile(ComDockerCli)
	if err != nil {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key != "-ldflags" {
			continue
		}
		args, err := shlex.Split(s.Value)
		if err != nil {
			return ""
		}
		for _, a := range args {
			// https://github.com/docker/cli/blob/f1615facb1ca44e4336ab20e621315fc2cfb845a/scripts/build/.variables#L77
			if !strings.HasPrefix(a, "github.com/docker/cli/cli/version.Version") {
				continue
			}
			parts := strings.Split(a, "=")
			if len(parts) != 2 {
				return ""
			}
			return parts[1]
		}
	}
	return ""
}

// ExecSilent executes a command and do redirect output to stdOut, return output
func ExecSilent(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 {
		args = os.Args[1:]
	}
	cmd := exec.CommandContext(ctx, comDockerCli(), args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

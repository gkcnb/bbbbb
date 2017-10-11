package cluster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type cluster struct {
	DefaultMemory       string
	DefaultDisk         string
	DefaultStdoutStderr io.Writer
}

func New(language, memory, disk string, out io.Writer) models.Cf {
	return &cluster{
		Language: language
		DefaultMemory: memory
		DefaultDisk: disk
		DefaultStdoutStderr: out
	}
}

var _ = models.Cf(&cluster{})

type app struct {
	name       string
	Path       string
	Stack      string
	Buildpacks []string
	Memory     string
	Disk       string
	stdout     *bytes.Buffer
	appGUID    string
	env        map[string]string
	logCmd     *exec.Cmd
}

var _ = models.CfApp(&app{})

func (c *cluster) New(fixture string) models.CfApp {
	return &app{
		name:       filepath.Base(fixture) + "-" + RandStringRunes(20),
		Path:       fixture,
		Stack:      "",
		Buildpacks: []string{},
		Memory:     c.DefaultMemory,
		Disk:       c.DefaultDisk,
		appGUID:    "",
		env:        map[string]string{},
		logCmd:     nil,
	}
}

func (c *cluster) HasTask() (bool,error) {
	return c.ApiVersionGT("2.75.0")
}
func (c *cluster) HasMultiBuildpack() (bool, error) {
	return c.ApiVersionGT("3.27.0")
}

func (c *cluster) apiVersionGT(minVersion string) (bool, error) {
	apiVersionString, err := cutlass.ApiVersion()
	if err != nil { return err }
	apiVersion, err := semver.Make(apiVersionString)
	if err != nil { return err }
	minRange, err := semver.ParseRange(">= "+minVersion)
	if err != nil { return err }
	return minRange(apiVersion)
}

func (c *cluster) apiVersion() (string, error) {
	cmd := exec.Command("cf", "curl", "/v2/info")
	cmd.Stderr = DefaultStdoutStderr
	bytes, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var info struct {
		ApiVersion string `json:"api_version"`
	}
	if err := json.Unmarshal(bytes, &info); err != nil {
		return "", err
	}
	return info.ApiVersion, nil
}

func (c *cluster) Cleanup() error {
	if err := c.deleteOrphanedRoutes(); err != nil {
		return err
	}
	// FIXME
	// if err := c.deleteBuildpack(); err != nil {
	// 	return err
	// }

	return nil
}

func (c *cluster) deleteOrphanedRoutes() error {
	command := exec.Command("cf", "delete-orphaned-routes", "-f")
	command.Stdout = DefaultStdoutStderr
	command.Stderr = DefaultStdoutStderr
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}

// func (c *cluster) deleteBuildpack(language string) error {
// 	command := exec.Command("cf", "delete-buildpack", "-f", fmt.Sprintf("%s_buildpack", language))
// 	if data, err := command.CombinedOutput(); err != nil {
// 		fmt.Println(string(data))
// 		return err
// 	}
// 	return nil
// }

func (c *cluster) updateBuildpack(language, file string) error {
	command := exec.Command("cf", "update-buildpack", fmt.Sprintf("%s_buildpack", language), "-p", file, "--enable")
	if data, err := command.CombinedOutput(); err != nil {
		return err
	}
	return nil
}

func (c *cluster) createBuildpack(language, file string) error {
	command := exec.Command("cf", "create-buildpack", fmt.Sprintf("%s_buildpack", language), file, "100", "--enable")
	if _, err := command.CombinedOutput(); err != nil {
		fmt.Println(string(data))
		return err
	}
	return nil
}

func (c *cluster) Buildpack(file string) error {
	if err := updateBuildpack(c.Language, file); err == nil {
		return nil
	}
	return createBuildpack(c.Language, file)
}

func (a *App) Name() string {
	return a.name
}

func (a *App) RunTask(command string) ([]byte, error) {
	cmd := exec.Command("cf", "run-task", a.name, command)
	cmd.Stderr = DefaultStdoutStderr
	bytes, err := cmd.Output()
	if err != nil {
		return bytes, err
	}
	return bytes, nil
}

func (a *App) Restart() error {
	command := exec.Command("cf", "restart", a.name)
	command.Stdout = DefaultStdoutStderr
	command.Stderr = DefaultStdoutStderr
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}

func (a *App) SetEnv(key, value string) {
	a.env[key] = value
}

func (a *App) SpaceGUID() (string, error) {
	cfHome := os.Getenv("CF_HOME")
	if cfHome == "" {
		cfHome = os.Getenv("HOME")
	}
	bytes, err := ioutil.ReadFile(filepath.Join(cfHome, ".cf", "config.json"))
	if err != nil {
		return "", err
	}
	var config cfConfig
	if err := json.Unmarshal(bytes, &config); err != nil {
		return "", err
	}
	return config.SpaceFields.GUID, nil
}

func (a *App) AppGUID() (string, error) {
	if a.appGUID != "" {
		return a.appGUID, nil
	}
	guid, err := a.SpaceGUID()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("cf", "curl", "/v2/apps?q=space_guid:"+guid+"&q=name:"+a.name)
	cmd.Stderr = DefaultStdoutStderr
	bytes, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var apps cfApps
	if err := json.Unmarshal(bytes, &apps); err != nil {
		return "", err
	}
	if len(apps.Resources) != 1 {
		return "", fmt.Errorf("Expected one app, found %d", len(apps.Resources))
	}
	a.appGUID = apps.Resources[0].Metadata.GUID
	return a.appGUID, nil
}

func (a *App) InstanceStates() ([]string, error) {
	guid, err := a.AppGUID()
	if err != nil {
		return []string{}, err
	}
	cmd := exec.Command("cf", "curl", "/v2/apps/"+guid+"/instances")
	cmd.Stderr = DefaultStdoutStderr
	bytes, err := cmd.Output()
	if err != nil {
		return []string{}, err
	}
	var data map[string]cfInstance
	if err := json.Unmarshal(bytes, &data); err != nil {
		return []string{}, err
	}
	var states []string
	for _, value := range data {
		states = append(states, value.State)
	}
	return states, nil
}

func (a *App) IsRunning(max int) bool {
	timeout := time.After(max * time.Second)
	tick := time.Tick(500 * time.Millisecond)
	for {
		select {
		case <-timeout:
			return false
		case <-tick:
			if states,err := app.InstanceStates(); err == nil {
				if len(states) == 1 && states[0] == "RUNNING" {
					return true
				}
			}
		}
	}
}

func (a *App) Push() error {
	args := []string{"push", a.name, "--no-start", "-p", a.Path}
	if a.Stack != "" {
		args = append(args, "-s", a.Stack)
	}
	if len(a.Buildpacks) == 1 {
		args = append(args, "-b", a.Buildpacks[len(a.Buildpacks)-1])
	}
	if _, err := os.Stat(filepath.Join(a.Path, "manifest.yml")); err == nil {
		args = append(args, "-f", filepath.Join(a.Path, "manifest.yml"))
	}
	if a.Memory != "" {
		args = append(args, "-m", a.Memory)
	}
	if a.Disk != "" {
		args = append(args, "-k", a.Disk)
	}
	command := exec.Command("cf", args...)
	command.Stdout = DefaultStdoutStderr
	command.Stderr = DefaultStdoutStderr
	if err := command.Run(); err != nil {
		return err
	}

	for k, v := range a.env {
		command := exec.Command("cf", "set-env", a.name, k, v)
		command.Stdout = DefaultStdoutStderr
		command.Stderr = DefaultStdoutStderr
		if err := command.Run(); err != nil {
			return err
		}
	}

	a.logCmd = exec.Command("cf", "logs", a.name)
	a.logCmd.Stderr = DefaultStdoutStderr
	a.Stdout = bytes.NewBuffer(nil)
	a.logCmd.Stdout = a.Stdout
	if err := a.logCmd.Start(); err != nil {
		return err
	}

	if len(a.Buildpacks) > 1 {
		args = []string{"v3-push", a.name, "-p", a.Path}
		for _, buildpack := range a.Buildpacks {
			args = append(args, "-b", buildpack)
		}
	} else {
		args = []string{"start", a.name}
	}
	command = exec.Command("cf", args...)
	command.Stdout = DefaultStdoutStderr
	command.Stderr = DefaultStdoutStderr
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}

func (a *App) GetUrl(path string) (string, error) {
	guid, err := a.AppGUID()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("cf", "curl", "/v2/apps/"+guid+"/summary")
	cmd.Stderr = DefaultStdoutStderr
	data, err := cmd.Output()
	if err != nil {
		return "", err
	}
	host := gjson.Get(string(data), "routes.0.host").String()
	domain := gjson.Get(string(data), "routes.0.domain.name").String()
	return fmt.Sprintf("http://%s.%s%s", host, domain, path), nil
}

func (a *App) Get(path string, headers map[string]string) (string, map[string][]string, error) {
	url, err := a.GetUrl(path)
	if err != nil {
		return "", map[string][]string{}, err
	}
	client := &http.Client{}
	if headers["NoFollow"] == "true" {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		delete(headers, "NoFollow")
	}
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range headers {
		req.Header.Add(k, v)
	}
	if headers["user"] != "" && headers["password"] != "" {
		req.SetBasicAuth(headers["user"], headers["password"])
		delete(headers, "user")
		delete(headers, "password")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", map[string][]string{}, err
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", map[string][]string{}, err
	}
	resp.Header["StatusCode"] = []string{strconv.Itoa(resp.StatusCode)}
	return string(data), resp.Header, err
}

func (a *App) GetBody(path string) (string, error) {
	body, _, err := a.Get(path, map[string]string{})
	// TODO: Non 200 ??
	// if !(len(headers["StatusCode"]) == 1 && headers["StatusCode"][0] == "200") {
	// 	return "", fmt.Errorf("non 200 status: %v", headers)
	// }
	return body, err
}

func (a *App) Files(path string) ([]string, error) {
	cmd := exec.Command("cf", "ssh", a.name, "-c", "find "+path)
	cmd.Stderr = DefaultStdoutStderr
	output, err := cmd.Output()
	if err != nil {
		return []string{}, err
	}
	return strings.Split(string(output), "\n"), nil
}

func (a *App) Destroy() error {
	if a.logCmd != nil && a.logCmd.Process != nil {
		if err := a.logCmd.Process.Kill(); err != nil {
			return err
		}
	}

	command := exec.Command("cf", "delete", "-f", a.name)
	command.Stdout = DefaultStdoutStderr
	command.Stderr = DefaultStdoutStderr
	return command.Run()
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz")

func RandStringRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

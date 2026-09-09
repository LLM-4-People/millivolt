// Command containercheck owns only disposable, network-isolated Docker fixtures.
// The host dev proxy lifecycle remains exclusively owned by scripts/dev.sh.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

const (
	smokeAddress = "http://127.0.0.1:8080" // Container loopback only, after isolation guards.
	smokeEnv     = "MILLIVOLT_CONTAINER_SMOKE"
	smokeRows    = 321 // Neutral persisted Settings fixture.
	// Arms the image's operator gate for the smoke: the probe asserts the
	// same mutation is denied without the credential and succeeds with it.
	smokeToken   = "millivolt-container-smoke-token"
	smokeTimeout = 45 * time.Second // Internal smoke-test deadline, not server configuration.
)

var imageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func main() {
	image := flag.String("image", "", "already-loaded local image ID")
	inside := flag.String("inside", "", "internal container phase")
	flag.Parse()
	var err error
	if flag.NArg() != 0 {
		err = errors.New("unexpected arguments")
	} else if *inside != "" {
		err = probe(*inside)
	} else {
		err = check(*image)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "container check:", err)
		os.Exit(1)
	}
}

// Docker archive streams are binary: never trim them or mix stderr into tar.
// Text commands reuse this boundary and may combine its separate diagnostics.
func dockerCommandIO(ctx context.Context, env []string, input io.Reader, output io.Writer, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env, cmd.Stdin, cmd.Stdout = env, input, output
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stderr.Bytes(), nil
}

func dockerCommand(ctx context.Context, env []string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	diagnostics, err := dockerCommandIO(ctx, env, nil, &stdout, args...)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(append(stdout.Bytes(), diagnostics...)), nil
}

type dockerCLI struct {
	endpoint string
	env      []string
}

func (d dockerCLI) command(ctx context.Context, args ...string) ([]byte, error) {
	return dockerCommand(ctx, d.env, append([]string{"--host", d.endpoint}, args...)...)
}

// copyVolumeArchive exercises the image-only recovery path. The caller owns
// both stopped containers and a fresh archive directory. --archive retains the
// numeric runtime UID/GID, including the private Settings file's mode and owner.
func (d dockerCLI) copyVolumeArchive(ctx context.Context, source, target, volume, path string) (err error) {
	archive, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, archive.Close()) }()
	if _, err := dockerCommandIO(ctx, d.env, nil, archive, "--host", d.endpoint, "cp", source+":"+volume+"/.", "-"); err != nil {
		return err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = dockerCommandIO(ctx, d.env, archive, io.Discard, "--host", d.endpoint, "cp", "--archive", "-", target+":"+volume)
	return err
}

func (d dockerCLI) stopClean(ctx context.Context, container string) error {
	if _, err := d.command(ctx, "stop", "--time", "30", container); err != nil {
		return err
	}
	state, err := d.command(ctx, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", container)
	if err != nil {
		return err
	}
	if string(state) != "exited 0" {
		return fmt.Errorf("unclean container stop %s", state)
	}
	return nil
}

func localDaemon(ctx context.Context) (dockerCLI, error) {
	// DOCKER_CONTEXT overrides DOCKER_HOST. Resolve once, then pin the verified
	// endpoint on every operation, including cleanup, regardless of later context changes.
	host := os.Getenv("DOCKER_HOST")
	if selected := os.Getenv("DOCKER_CONTEXT"); selected != "" || host == "" {
		args := []string{"context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}
		if selected != "" {
			args = append(args, selected)
		}
		endpoint, err := dockerCommand(ctx, os.Environ(), args...)
		if err != nil {
			return dockerCLI{}, err
		}
		host = string(endpoint)
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "unix" || u.Host != "" || !filepath.IsAbs(u.Path) || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return dockerCLI{}, errors.New("only a local absolute Unix-socket Docker endpoint is allowed")
	}
	env := slices.DeleteFunc(os.Environ(), func(value string) bool {
		key, _, _ := strings.Cut(value, "=")
		return key == "DOCKER_HOST" || key == "DOCKER_CONTEXT" || key == "DOCKER_TLS" || key == "DOCKER_TLS_VERIFY" || key == "DOCKER_CERT_PATH"
	})
	return dockerCLI{endpoint: host, env: env}, nil
}
func check(image string) error {
	if runtime.GOOS != "linux" || !imageID.MatchString(image) {
		return errors.New("requires Linux and an already-loaded sha256 image ID")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 4*smokeTimeout)
	defer cancel()
	daemon, err := localDaemon(ctx)
	if err != nil {
		return err
	}
	if err := daemon.checkCompose(ctx); err != nil {
		return err
	}
	command := daemon.command
	user, err := command(ctx, "image", "inspect", "--format", "{{.Config.User}}", image)
	if err != nil {
		return err
	}
	if string(user) != "65532:65532" {
		return fmt.Errorf("unexpected image user %q", user)
	}
	arch, err := command(ctx, "image", "inspect", "--format", "{{.Architecture}}", image)
	if err != nil {
		return err
	}
	if string(arch) != runtime.GOARCH {
		return errors.New("smoke requires a native image")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	token := make([]byte, 12)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	name := "millivolt-smoke-" + hex.EncodeToString(token)
	var created, containers []string
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), smokeTimeout)
		defer cancel()
		// Every target is a literal ID minted by this invocation, never a user path.
		for _, container := range containers {
			if _, err := command(cleanup, "rm", "--force", container); err != nil {
				fmt.Fprintln(os.Stderr, "container cleanup:", err)
			}
		}
		for _, volume := range created {
			if _, err := command(cleanup, "volume", "rm", volume); err != nil {
				fmt.Fprintln(os.Stderr, "volume cleanup:", err)
			}
		}
	}()
	create := func(name string) (string, error) {
		volumes := []string{name + "-data", name + "-config"}
		for _, volume := range volumes {
			if _, err := command(ctx, "volume", "create", volume); err != nil {
				return "", err
			}
			created = append(created, volume)
		}
		args := []string{"create", "--pull", "never", "--name", name, "--network", "none", "--read-only", "--cap-drop", "ALL",
			"--security-opt", "no-new-privileges", "--env", smokeEnv + "=1",
			"--env", "MILLIVOLT_OPERATOR_TOKEN=" + smokeToken,
			// /tmp is deliberately far smaller than a database snapshot: operator
			// backup and restore must stage next to db_path on the data volume,
			// never on tmpfs (the 16m deployment default stays meaningful).
			"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=64k",
			"--mount", "type=volume,source=" + volumes[0] + ",target=/data",
			"--mount", "type=volume,source=" + volumes[1] + ",target=/config",
			"--mount", "type=bind,source=" + executable + ",target=/containercheck,readonly", image}
		containerID, err := command(ctx, args...)
		if err != nil {
			return "", err
		}
		if !imageID.MatchString("sha256:" + string(containerID)) {
			return "", errors.New("Docker did not return a container ID")
		}
		container := string(containerID)
		containers = append(containers, container)
		return container, nil
	}
	defer func() {
		logsContext, cancel := context.WithTimeout(context.Background(), smokeTimeout)
		defer cancel()
		for _, container := range containers {
			if logs, err := command(logsContext, "logs", container); err == nil {
				fmt.Fprintln(os.Stderr, string(logs))
			}
		}
	}()
	container, err := create(name)
	if err != nil {
		return err
	}
	if _, err := command(ctx, "start", container); err != nil {
		return err
	}
	for _, phase := range []string{"seed", "verify"} {
		if _, err := command(ctx, "exec", container, "/containercheck", "-inside", phase); err != nil {
			return err
		}
		if phase == "seed" {
			// The distroless image has no shell or curl: the binary probes
			// its own unauthenticated /healthz for Docker HEALTHCHECK.
			if _, err := command(ctx, "exec", container, "/millivolt", "-config", "/config/proxy.yaml", "-db-path", "/data/proxy.db", "-healthcheck"); err != nil {
				return fmt.Errorf("healthcheck probe: %w", err)
			}
			version, err := command(ctx, "exec", container, "/millivolt", "-version")
			if err != nil {
				return err
			}
			var info struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal(version, &info); err != nil {
				return err
			}
			want, err := os.ReadFile("VERSION")
			if err != nil {
				return err
			}
			if info.Version != strings.TrimSpace(string(want)) {
				return fmt.Errorf("image version %q differs from VERSION", info.Version)
			}
			for _, output := range []struct {
				flag          string
				configuration func() *config.Config
			}{
				{"-print-config", config.Default},
				{"-print-example-config", config.Example},
			} {
				printed, err := command(ctx, "exec", container, "/millivolt", output.flag)
				if err != nil {
					return err
				}
				if err := checkConfigYAML(printed, output.configuration()); err != nil {
					return fmt.Errorf("image %s: %w", output.flag, err)
				}
			}
			example, err := os.ReadFile("proxy.example.yaml")
			if err != nil {
				return err
			}
			if err := checkConfigYAML(example, config.Example()); err != nil {
				return fmt.Errorf("committed example: %w", err)
			}
		}
		if err := daemon.stopClean(ctx, container); err != nil {
			return err
		}
		if phase == "seed" {
			if _, err := command(ctx, "start", container); err != nil {
				return err
			}
		}
	}
	// The original stop/start/verify smoke remains above. Recovery uses another
	// stopped container with fresh volumes, never the source volumes or a helper
	// image, and must boot with the same saved settings and single durable row.
	restored, err := create(name + "-restored")
	if err != nil {
		return err
	}
	archiveDir, err := os.MkdirTemp("", "millivolt-container-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(archiveDir) // Only this invocation's private scratch tree.
	for _, volume := range []string{"/data", "/config"} {
		if err := daemon.copyVolumeArchive(ctx, container, restored, volume, filepath.Join(archiveDir, filepath.Base(volume)+".tar")); err != nil {
			return err
		}
	}
	if _, err := command(ctx, "start", restored); err != nil {
		return err
	}
	if _, err := command(ctx, "exec", restored, "/containercheck", "-inside", "verify"); err != nil {
		return fmt.Errorf("restored container: %w", err)
	}
	if err := daemon.stopClean(ctx, restored); err != nil {
		return err
	}
	fmt.Printf("container HTTP/config/SQLite/restart/backup/restore smoke passed (%s)\n", runtime.GOARCH)
	return nil
}

func (d dockerCLI) checkCompose(ctx context.Context) error {
	// Validate the committed deployment, not operator environment/override files.
	d.env = slices.DeleteFunc(slices.Clone(d.env), func(value string) bool {
		return strings.HasPrefix(value, "COMPOSE_") || strings.HasPrefix(value, "MILLIVOLT_")
	})
	var models [][]byte
	for _, file := range []string{"compose.yaml", "compose.dev.yaml"} {
		data, err := d.command(ctx, "compose", "--env-file", os.DevNull, "--file", file, "config", "--format", "json")
		if err != nil {
			return err
		}
		models = append(models, data)
	}
	return validateCompose(models[0], models[1])
}

type composeModel struct {
	Name     string `json:"name"`
	Services map[string]struct {
		Image         string `json:"image"`
		ContainerName string `json:"container_name"`
		Build         *struct {
			Context string `json:"context"`
		} `json:"build"`
		PullPolicy string `json:"pull_policy"`
		ReadOnly   bool   `json:"read_only"`
		Ports      []struct {
			HostIP    string `json:"host_ip"`
			Published string `json:"published"`
			Target    int    `json:"target"`
		} `json:"ports"`
		Volumes []struct{ Type, Source, Target string } `json:"volumes"`
	} `json:"services"`
	Volumes map[string]struct {
		Name     string `json:"name"`
		External bool   `json:"external"`
	} `json:"volumes"`
}

func decodeCompose(data []byte) (composeModel, error) {
	var model composeModel
	if err := json.Unmarshal(data, &model); err != nil {
		return model, err
	}
	service, ok := model.Services[model.Name]
	if !ok || model.Name == "" || len(model.Services) != 1 || service.Image == "" || service.ContainerName != model.Name || !service.ReadOnly || len(service.Ports) != 1 || service.Ports[0].HostIP != "127.0.0.1" || service.Ports[0].Published == "" || service.Ports[0].Target != 8080 {
		return model, errors.New("Compose must contain one named, read-only, loopback-published service whose name matches the project and container")
	}
	if len(service.Volumes) != 2 || len(model.Volumes) != 2 {
		return model, errors.New("Compose must keep exactly two named persistence volumes")
	}
	seen, names := map[string]bool{}, map[string]bool{}
	for _, volume := range service.Volumes {
		definition := model.Volumes[volume.Source]
		if volume.Type != "volume" || definition.Name == "" || definition.External || names[definition.Name] || (volume.Target != "/data" && volume.Target != "/config") || seen[volume.Target] {
			return model, errors.New("Compose must keep separate project-owned /data and /config volumes")
		}
		seen[volume.Target] = true
		names[definition.Name] = true
	}
	return model, nil
}

func validateCompose(deployment, development []byte) error {
	normal, err := decodeCompose(deployment)
	if err != nil {
		return err
	}
	dev, err := decodeCompose(development)
	if err != nil {
		return err
	}
	normalService, devService := normal.Services[normal.Name], dev.Services[dev.Name]
	if normalService.Build != nil || devService.Build == nil || devService.Build.Context == "" || devService.PullPolicy != "build" {
		return errors.New("only the explicit development Compose may build source")
	}
	if normal.Name == dev.Name || normalService.Image == devService.Image || normalService.Ports[0].Published == devService.Ports[0].Published || devService.Ports[0].Published == "8080" {
		return errors.New("development Compose must isolate its project, image and published port")
	}
	for _, volume := range normal.Volumes {
		for _, devVolume := range dev.Volumes {
			if volume.Name == devVolume.Name {
				return errors.New("deployment and development Compose share persistence")
			}
		}
	}
	return nil
}
func probeGuard() error {
	if os.Getenv(smokeEnv) != "1" || os.Getuid() != 65532 {
		return errors.New("internal probe requires isolated nonroot container")
	}
	target, err := os.Readlink("/proc/1/exe")
	if err != nil || filepath.Clean(target) != "/millivolt" {
		return errors.New("PID 1 is not the fixture binary")
	}
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return errors.New("not a Docker container")
	}
	return nil
}

type snapshot struct {
	Records []struct {
		Client string `json:"client"`
		Model  string `json:"model"`
		Usage  struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	} `json:"records"`
	Storage struct {
		Enabled bool  `json:"enabled"`
		Dropped int64 `json:"dropped"`
	} `json:"storage"`
}
type settings struct {
	Revision  string         `json:"revision"`
	Values    map[string]any `json:"values"`
	Effective map[string]any `json:"effective"`
}

func checkConfigYAML(data []byte, configuration *config.Config) error {
	var generated bytes.Buffer
	if err := config.WriteYAML(&generated, configuration); err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(generated.Bytes())) {
		return errors.New("configuration differs from its canonical generator")
	}
	return nil
}

func readPrivateConfig(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("configuration must be a regular file with mode 0600")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Getuid() || int(owner.Gid) != os.Getgid() {
		return nil, errors.New("configuration must belong to the runtime user and group")
	}
	return os.ReadFile(path)
}

func checkExampleProviders(cfg settings) error {
	encoded, err := json.Marshal(config.Example().Map()["providers"])
	if err != nil {
		return err
	}
	var want any
	if err := json.Unmarshal(encoded, &want); err != nil {
		return err
	}
	for _, state := range []map[string]any{cfg.Values, cfg.Effective} {
		if !reflect.DeepEqual(state["providers"], want) {
			return errors.New("saved and effective provider profiles must match the shipped example")
		}
	}
	return nil
}

// do sends one smoke request and returns the status with the bounded body.
func do(client *http.Client, method, path string, body []byte, headers map[string]string) (int, []byte, error) {
	r, err := http.NewRequest(method, smokeAddress+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	res, err := client.Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return res.StatusCode, nil, err
	}
	return res.StatusCode, data, nil
}

func request(client *http.Client, method, path string, body []byte) ([]byte, error) {
	status, data, err := do(client, method, path, body, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s %s returned %d: %s", method, path, status, data)
	}
	return data, nil
}
func readJSON(client *http.Client, path string, out any) error {
	data, err := request(client, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// authTransport injects the operator credential into every smoke read: the
// dashboard plane is gated and the probe must behave like an operator
// browser. The negative checks deliberately use a plain client instead.
type authTransport struct {
	base  http.RoundTripper
	token string
}

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return a.base.RoundTrip(r)
}

func probe(phase string) error {
	if phase != "seed" && phase != "verify" {
		return errors.New("unknown probe phase")
	}
	if err := probeGuard(); err != nil {
		return err
	}
	// The whole dashboard plane is gated: every read below rides an
	// authenticated transport, while the negative checks use a plain client.
	credential := os.Getenv("MILLIVOLT_OPERATOR_TOKEN")
	if credential == "" {
		return errors.New("probe env is missing MILLIVOLT_OPERATOR_TOKEN")
	}
	client := &http.Client{Timeout: 3 * time.Second,
		Transport: authTransport{base: &http.Transport{Proxy: nil}, token: credential}}
	defer client.CloseIdleConnections()
	plain := &http.Client{Timeout: 3 * time.Second,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	defer plain.CloseIdleConnections()
	deadline := time.Now().Add(smokeTimeout)
	var snap snapshot
	for {
		if err := readJSON(client, "/metrics/bootstrap", &snap); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("container HTTP readiness timed out")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !snap.Storage.Enabled || snap.Storage.Dropped != 0 {
		return errors.New("durability unavailable or dropped records")
	}
	// The liveness probe and brand favicon are open server routes: no
	// credential, no dashboard data. The ungated dashboard entry denies
	// with the login page, whose handshake mints the session cookie
	// (EventSource cannot send Authorization headers).
	if status, _, err := do(plain, http.MethodGet, "/healthz", nil, nil); err != nil || status != http.StatusOK {
		return fmt.Errorf("healthz probe returned %d, %v", status, err)
	}
	if status, body, err := do(plain, http.MethodGet, "/favicon.ico", nil, nil); err != nil || status != http.StatusOK || len(body) < 4 || body[0] != 0 || body[1] != 0 || body[2] != 1 || body[3] != 0 {
		return fmt.Errorf("ungated favicon returned %d, %v", status, err)
	}
	if status, body, err := do(plain, http.MethodGet, "/manifest.webmanifest", nil, nil); err != nil || status != http.StatusOK || !bytes.Contains(body, []byte(`"start_url"`)) {
		return fmt.Errorf("ungated manifest returned %d, %v", status, err)
	}
	if status, body, err := do(plain, http.MethodGet, "/sw.js", nil, nil); err != nil || status != http.StatusOK || !bytes.Contains(body, []byte("addEventListener('fetch'")) {
		return fmt.Errorf("ungated service worker returned %d, %v", status, err)
	}
	if status, body, err := do(plain, http.MethodGet, "/icon-192.png", nil, nil); err != nil || status != http.StatusOK || len(body) < 8 || string(body[:8]) != "\x89PNG\r\n\x1a\n" {
		return fmt.Errorf("ungated icon-192 returned %d, %v", status, err)
	}
	status, login, err := do(plain, http.MethodGet, "/", nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusUnauthorized || !bytes.Contains(login, []byte(`action="/admin/session"`)) || !bytes.Contains(login, []byte(`rel="manifest"`)) {
		return fmt.Errorf("ungated dashboard returned %d, want the 401 login page", status)
	}
	session, err := http.NewRequest(http.MethodPost, smokeAddress+"/admin/session", strings.NewReader("token="+url.QueryEscape(credential)))
	if err != nil {
		return err
	}
	session.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := plain.Do(session)
	if err != nil {
		return err
	}
	cookies := res.Cookies()
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || len(cookies) != 1 {
		return fmt.Errorf("session handshake returned %d with %d cookies, want 303 + one cookie", res.StatusCode, len(cookies))
	}
	authed := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer authed.CloseIdleConnections()
	page, err := http.NewRequest(http.MethodGet, smokeAddress+"/", nil)
	if err != nil {
		return err
	}
	for _, cookie := range cookies {
		page.AddCookie(cookie)
	}
	served, err := authed.Do(page)
	if err != nil {
		return err
	}
	dash, err := io.ReadAll(io.LimitReader(served.Body, 4<<20))
	served.Body.Close()
	if err != nil {
		return err
	}
	if served.StatusCode != http.StatusOK || !bytes.Contains(dash, []byte("<html")) {
		return fmt.Errorf("session-cookie dashboard returned %d, want the HTML shell", served.StatusCode)
	}
	html, err := request(client, http.MethodGet, "/", nil)
	if err != nil {
		return err
	}
	if !bytes.Contains(html, []byte("<html")) {
		return errors.New("dashboard HTML missing")
	}
	var restart struct {
		Available bool `json:"available"`
	}
	if err := readJSON(client, "/admin/restart", &restart); err != nil {
		return err
	}
	if restart.Available {
		return errors.New("source-less image falsely offers rebuild")
	}
	var cfg settings
	if err := readJSON(client, "/admin/config", &cfg); err != nil {
		return err
	}
	if err := checkExampleProviders(cfg); err != nil {
		return err
	}
	configuration, err := readPrivateConfig("/config/proxy.yaml")
	if err != nil {
		return err
	}
	if phase == "seed" {
		if err := checkConfigYAML(configuration, config.Example()); err != nil {
			return fmt.Errorf("fresh config volume: %w", err)
		}
		if len(snap.Records) != 0 {
			return errors.New("fixture volume was not empty")
		}
		body, _ := json.Marshal(map[string]any{"revision": cfg.Revision, "values": map[string]any{"dash_log_rows": smokeRows}})
		// The operator gate is fail closed: the image arms it from the
		// environment, the identical mutation without the credential is
		// denied 401, and the authorized request carries that credential.
		status, _, err := do(plain, http.MethodPost, "/admin/config", body, nil)
		if err != nil {
			return err
		}
		if status != http.StatusUnauthorized {
			return fmt.Errorf("ungated operator mutation returned %d, want 401", status)
		}
		status, _, err = do(client, http.MethodPost, "/admin/config", body, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("authorized operator mutation returned %d, want 200", status)
		}
		const response = `{"id":"fixture","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, response)
		}))
		defer upstream.Close()
		r, err := http.NewRequest(http.MethodPost, smokeAddress+"/v1/chat/completions", strings.NewReader(`{"model":"fixture-model","messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			return err
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Proxy-Base-URL", upstream.URL)
		r.Header.Set("X-Proxy-Provider", "fixture.example")
		r.Header.Set("X-Proxy-Client", "container-fixture")
		// Transparent inference rides the unauthenticated client: the
		// operator plane never gates the relay, and the relay never needs
		// the operator credential.
		res, err := plain.Do(r)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return err
		}
		if res.StatusCode != 200 || string(data) != response {
			return fmt.Errorf("mock inference changed: %d %s", res.StatusCode, data)
		}
		// The Settings database archive rides this fixture: download packs a
		// VACUUM INTO snapshot and restore inspects the upload, both staging
		// database-sized transient files next to db_path on the data volume.
		// The 64k tmpfs cannot hold them, so a /tmp staging path fails here.
		archive, err := request(client, http.MethodGet, "/admin/backup?database=1", nil)
		if err != nil {
			return err
		}
		status, body, err = do(client, http.MethodPost, "/admin/restore?database=1", archive, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("settings database restore returned %d: %s", status, body)
		}
		var staged struct {
			OK              bool     `json:"ok"`
			RestartRequired []string `json:"restart_required"`
		}
		if err := json.Unmarshal(body, &staged); err != nil {
			return err
		}
		if !staged.OK || len(staged.RestartRequired) != 1 || staged.RestartRequired[0] != "db_path" {
			return fmt.Errorf("settings database restore json: %+v", staged)
		}
	}
	if err := readJSON(client, "/admin/config", &cfg); err != nil {
		return err
	}
	if cfg.Values["dash_log_rows"] != float64(smokeRows) || cfg.Effective["dash_log_rows"] != float64(smokeRows) {
		return errors.New("Settings did not persist and apply")
	}
	if err := checkExampleProviders(cfg); err != nil {
		return err
	}
	if err := readJSON(client, "/metrics/bootstrap", &snap); err != nil {
		return err
	}
	if len(snap.Records) != 1 || snap.Records[0].Model != "fixture-model" || snap.Records[0].Usage.TotalTokens != 5 {
		return fmt.Errorf("wrong durable fixture record: %+v", snap.Records)
	}
	data, err := os.ReadFile("/data/proxy.db")
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		return errors.New("SQLite file header missing")
	}
	return nil
}

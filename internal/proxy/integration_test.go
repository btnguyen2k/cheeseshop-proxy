package proxy

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/btnguyen2k/cheeseshop-proxy/internal/config"
	"github.com/btnguyen2k/cheeseshop-proxy/internal/upstream"
)

func TestPipInstall(t *testing.T) {
	if os.Getenv("CHEESESHOP_PIP_TEST") != "1" {
		t.Skip("set CHEESESHOP_PIP_TEST=1 to exercise real pip against offline fixtures")
	}
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Fatal("CHEESESHOP_PIP_TEST requires Python and pip on PATH")
	}
	nullDevice, err := exec.Command(python, "-c", "import os; print(os.devnull)").Output()
	if err != nil {
		t.Fatal(err)
	}
	runInstall := func(t *testing.T, app *Server, packages []string, success bool) string {
		t.Helper()
		directory := t.TempDir()
		target := filepath.Join(directory, "installed")
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		args := []string{"-m", "pip", "--disable-pip-version-check", "--no-input", "--no-cache-dir",
			"install", "--ignore-installed", "--no-compile", "--target", target,
			"--index-url", app.cfg.PublicBaseURL + "/simple/", "--trusted-host", "127.0.0.1"}
		command := exec.CommandContext(ctx, python, append(args, packages...)...)
		command.Dir = directory
		for _, setting := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(setting), "PIP_") {
				command.Env = append(command.Env, setting)
			}
		}
		command.Env = append(command.Env, "PIP_CONFIG_FILE="+strings.TrimSpace(string(nullDevice)))
		output, err := command.CombinedOutput()
		if ctx.Err() != nil || (err == nil) != success {
			t.Fatalf("pip install success=%v, want %v: %v\n%s", err == nil, success, err, output)
		}
		if success {
			module := strings.ReplaceAll(strings.ToLower(packages[0]), ".", "_")
			module = strings.ReplaceAll(module, "-", "_")
			if _, err := os.Stat(filepath.Join(target, module, "__init__.py")); err != nil {
				t.Fatalf("pip did not install the requested fixture: %v\n%s", err, output)
			}
		}
		return string(output)
	}
	newPackages := func(t *testing.T) *fixture {
		f := newFixture(t)
		addWheel(t, f, "company-dependency", "1.0.0", nil, ">=3.8", false)
		addWheel(t, f, "company-root", "1.0.0", []string{"company-dependency==1.0.0"}, ">=3.8", false)
		addWheel(t, f, "company-root", "2.0.0", nil, ">=99", false)
		addWheel(t, f, "company-root", "3.0.0", nil, ">=3.8", "withdrawn fixture")
		addWheel(t, f, "company-build", "1.0.0", nil, ">=3.8", false)
		addSource(t, f)
		return f
	}
	f := newPackages(t)
	app := startFixtureServer(t, f, []string{"company-*"})
	t.Run("wheel and runtime dependency", func(t *testing.T) {
		runInstall(t, app, []string{"Company.Root"}, true)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.calls[wheelURL("company-root", "2.0.0")] != 0 || f.calls[wheelURL("company-root", "3.0.0")] != 0 {
			t.Fatal("pip selected an incompatible or yanked release")
		}
		if f.calls[wheelURL("company-root", "1.0.0")+".metadata"] == 0 ||
			f.calls[wheelURL("company-dependency", "1.0.0")] == 0 {
			t.Fatal("pip did not use proxied core metadata and dependency artifacts")
		}
	})
	t.Run("warm proxy with empty client cache", func(t *testing.T) {
		calls := f.totalCalls()
		runInstall(t, app, []string{"Company.Root"}, true)
		if f.totalCalls() != calls {
			t.Fatal("warm-cache pip install fetched upstream again")
		}
	})
	t.Run("source distribution and build dependency", func(t *testing.T) {
		runInstall(t, app, []string{"company-source"}, true)
	})
	t.Run("excluded project", func(t *testing.T) {
		runInstall(t, app, []string{"outside-subset"}, false)
		if f.callCount(upstream.IndexURL+"outside-subset/") != 0 {
			t.Fatal("excluded project reached upstream")
		}
	})
	t.Run("runtime dependency requires its own rule", func(t *testing.T) {
		restricted := newPackages(t)
		app := startFixtureServer(t, restricted, []string{"company-root"})
		output := runInstall(t, app, []string{"company-root"}, false)
		if !strings.Contains(output, "company-dependency") || restricted.callCount(upstream.IndexURL+"company-dependency/") != 0 {
			t.Fatalf("runtime dependency was implicitly allowed:\n%s", output)
		}
	})
	t.Run("build dependency requires its own rule", func(t *testing.T) {
		restricted := newPackages(t)
		app := startFixtureServer(t, restricted, []string{"company-source"})
		output := runInstall(t, app, []string{"company-source"}, false)
		if !strings.Contains(output, "company-build") || restricted.callCount(upstream.IndexURL+"company-build/") != 0 {
			t.Fatalf("build dependency was implicitly allowed:\n%s", output)
		}
	})
}

func startFixtureServer(t *testing.T, f *fixture, allow []string) *Server {
	t.Helper()
	host := httptest.NewUnstartedServer(nil)
	app := testServer(t, f, func(cfg *config.Config) {
		cfg.PublicBaseURL = "http://" + host.Listener.Addr().String() + "/python"
		cfg.Allow = allow
	})
	host.Config.Handler = app
	host.Start()
	t.Cleanup(host.Close)
	response, err := http.Get(host.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("fixture server is not responsive")
	}
	return app
}

func wheelFilename(name, version string) string {
	return strings.ReplaceAll(name, "-", "_") + "-" + version + "-py3-none-any.whl"
}

func wheelURL(name, version string) string {
	return "https://files.pythonhosted.org/packages/fixture/" + wheelFilename(name, version)
}

func addWheel(t *testing.T, f *fixture, name, version string, dependencies []string, requiresPython string, yanked any) {
	t.Helper()
	module := strings.ReplaceAll(name, "-", "_")
	info := module + "-" + version + ".dist-info/"
	metadata := fmt.Sprintf("Metadata-Version: 2.1\nName: %s\nVersion: %s\nRequires-Python: %s\n", name, version, requiresPython)
	for _, dependency := range dependencies {
		metadata += "Requires-Dist: " + dependency + "\n"
	}
	metadata += "\n"
	files := map[string]string{
		info + "METADATA":       metadata,
		info + "WHEEL":          "Wheel-Version: 1.0\nGenerator: cheeseshop-fixtures\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		module + "/__init__.py": "__version__ = " + fmt.Sprintf("%q", version) + "\n",
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var record strings.Builder
	for _, name := range names {
		data := []byte(files[name])
		sum := sha256.Sum256(data)
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", name, base64.RawURLEncoding.EncodeToString(sum[:]), len(data))
	}
	fmt.Fprintf(&record, "%sRECORD,,\n", info)
	files[info+"RECORD"] = record.String()
	names = append(names, info+"RECORD")
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, name := range names {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	size := int64(buffer.Len())
	file := distribution{
		Filename: wheelFilename(name, version), URL: wheelURL(name, version), Size: &size,
		Hashes: map[string]string{"sha256": checksum(buffer.Bytes())}, RequiresPython: &requiresPython,
		CoreMetadata: map[string]string{"sha256": checksum([]byte(metadata))}, Yanked: yanked,
	}
	f.replies[file.URL] = fixtureReply{status: 200, body: buffer.Bytes(), header: http.Header{"Content-Type": {"application/octet-stream"}}}
	f.replies[file.URL+".metadata"] = fixtureReply{status: 200, body: []byte(metadata), header: http.Header{"Content-Type": {"text/plain"}}}
	addDistribution(t, f, name, version, file)
}

func addDistribution(t *testing.T, f *fixture, name, version string, file distribution) {
	t.Helper()
	document := projectDocument{Meta: apiMeta{Version: "1.4"}, Name: name, Files: []distribution{}, Versions: []string{}}
	if reply, ok := f.replies[upstream.IndexURL+name+"/"]; ok {
		if err := json.Unmarshal(reply.body, &document); err != nil {
			t.Fatal(err)
		}
	}
	document.Files = append(document.Files, file)
	document.Versions = append(document.Versions, version)
	f.setJSON(upstream.IndexURL+name+"/", document)
	var listing projectList
	if err := json.Unmarshal(f.replies[upstream.IndexURL].body, &listing); err != nil {
		t.Fatal(err)
	}
	listing.Projects = append(listing.Projects, projectName{Name: name})
	f.setJSON(upstream.IndexURL, listing)
}

func addSource(t *testing.T, f *fixture) {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	for name, content := range map[string]string{
		"pyproject.toml": "[build-system]\nrequires = [\"company-build==1.0.0\"]\nbuild-backend = \"backend\"\nbackend-path = [\".\"]\n",
		"backend.py":     sourceBackend,
		"PKG-INFO":       "Metadata-Version: 2.1\nName: company-source\nVersion: 1.0.0\n\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: "company_source-1.0.0/" + name, Mode: 0644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	size := int64(buffer.Len())
	file := distribution{
		Filename: "company_source-1.0.0.tar.gz",
		URL:      "https://files.pythonhosted.org/packages/fixture/company_source-1.0.0.tar.gz",
		Hashes:   map[string]string{"sha256": checksum(buffer.Bytes())}, Size: &size,
	}
	f.replies[file.URL] = fixtureReply{status: 200, body: buffer.Bytes(), header: http.Header{"Content-Type": {"application/octet-stream"}}}
	addDistribution(t, f, "company-source", "1.0.0", file)
}

const sourceBackend = `
import base64
import hashlib
import pathlib
import zipfile

INFO = "company_source-1.0.0.dist-info"
METADATA = "Metadata-Version: 2.1\nName: company-source\nVersion: 1.0.0\n\n"
WHEEL = "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n"

def prepare_metadata_for_build_wheel(metadata_directory, config_settings=None):
    directory = pathlib.Path(metadata_directory) / INFO
    directory.mkdir()
    (directory / "METADATA").write_text(METADATA, encoding="utf-8")
    (directory / "WHEEL").write_text(WHEEL, encoding="utf-8")
    return INFO

def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    filename = "company_source-1.0.0-py3-none-any.whl"
    files = {
        INFO + "/METADATA": METADATA,
        INFO + "/WHEEL": WHEEL,
        "company_source/__init__.py": '__version__ = "1.0.0"\n',
    }
    record = []
    for name, text in files.items():
        data = text.encode("utf-8")
        digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
        record.append(f"{name},sha256={digest},{len(data)}\n")
    record.append(INFO + "/RECORD,,\n")
    files[INFO + "/RECORD"] = "".join(record)
    with zipfile.ZipFile(pathlib.Path(wheel_directory) / filename, "w") as archive:
        for name, text in files.items():
            archive.writestr(name, text)
    return filename
`

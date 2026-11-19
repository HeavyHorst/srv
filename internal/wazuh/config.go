package wazuh

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Config is the controller's Wazuh configuration, read from the environment.
// The API password comes from SRV_WAZUH_API_PASSWORD_FILE (preferred) or
// SRV_WAZUH_API_PASSWORD; it is never stored in the repository.
type Config struct {
	APIURL         string // SRV_WAZUH_API_URL, https://host:55000
	APIUser        string // SRV_WAZUH_API_USER
	APIPassword    string // SRV_WAZUH_API_PASSWORD(_FILE)
	APICAFile      string // SRV_WAZUH_API_CA_FILE, PEM bundle trusted for the API
	ManagerAddress string // SRV_WAZUH_MANAGER_ADDRESS, what agents connect to
	ManagerPort    int    // SRV_WAZUH_MANAGER_PORT, default 1514
	StatePath      string // SRV_WAZUH_STATE, registration store
	// AssetPrefix (SRV_WAZUH_ASSET_PREFIX) prefixes a guest's name to form
	// its asset ID and agent name, so srv hosts sharing a manager stay
	// distinct, e.g. "gpu-rig-".
	AssetPrefix string
}

// Configured reports whether the environment asks for Wazuh enrollment.
func Configured(getenv func(string) string) bool {
	return getenv("SRV_WAZUH_API_URL") != ""
}

var prefixRE = regexp.MustCompile(`^[A-Za-z0-9._-]{0,64}$`)

// ConfigFromEnv reads Config from getenv.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{
		APIURL:         strings.TrimRight(getenv("SRV_WAZUH_API_URL"), "/"),
		APIUser:        getenv("SRV_WAZUH_API_USER"),
		APICAFile:      getenv("SRV_WAZUH_API_CA_FILE"),
		ManagerAddress: getenv("SRV_WAZUH_MANAGER_ADDRESS"),
		ManagerPort:    1514,
		StatePath:      getenv("SRV_WAZUH_STATE"),
		AssetPrefix:    getenv("SRV_WAZUH_ASSET_PREFIX"),
	}
	if !prefixRE.MatchString(c.AssetPrefix) {
		return Config{}, fmt.Errorf("invalid SRV_WAZUH_ASSET_PREFIX %q", c.AssetPrefix)
	}
	if port := getenv("SRV_WAZUH_MANAGER_PORT"); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return Config{}, fmt.Errorf("invalid SRV_WAZUH_MANAGER_PORT %q", port)
		}
		c.ManagerPort = p
	}
	file, inline := getenv("SRV_WAZUH_API_PASSWORD_FILE"), getenv("SRV_WAZUH_API_PASSWORD")
	switch {
	case file != "" && inline != "":
		return Config{}, errors.New("set only one of SRV_WAZUH_API_PASSWORD_FILE and SRV_WAZUH_API_PASSWORD")
	case file != "":
		info, err := os.Stat(file)
		if err != nil {
			return Config{}, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return Config{}, fmt.Errorf("%s must not be readable by group or others", file)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return Config{}, err
		}
		c.APIPassword = strings.TrimRight(string(b), "\r\n")
	default:
		c.APIPassword = inline
	}
	var missing []string
	for name, v := range map[string]string{
		"SRV_WAZUH_API_URL": c.APIURL, "SRV_WAZUH_API_USER": c.APIUser, "SRV_WAZUH_API_PASSWORD(_FILE)": c.APIPassword,
		"SRV_WAZUH_MANAGER_ADDRESS": c.ManagerAddress, "SRV_WAZUH_STATE": c.StatePath,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(c.APIURL, "https://") {
		return Config{}, errors.New("SRV_WAZUH_API_URL must be https")
	}
	return c, nil
}

// Coordinator builds a Coordinator using the manager API and a FileStore.
func (c Config) Coordinator() (*Coordinator, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.APICAFile != "" {
		pem, err := os.ReadFile(c.APICAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificates", c.APICAFile)
		}
		tlsConfig.RootCAs = pool
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	return &Coordinator{
		Manager:        &HTTPManager{BaseURL: c.APIURL, Client: client, Username: c.APIUser, Password: c.APIPassword},
		Store:          &FileStore{Path: c.StatePath},
		ManagerAddress: c.ManagerAddress,
		ManagerPort:    c.ManagerPort,
	}, nil
}

// FileStore keeps registrations in one JSON file. An exclusive flock on a
// sibling lock file serialises every access, so controllers on one host
// share its compare-and-swap fencing.
type FileStore struct {
	Path string
	mu   sync.Mutex
}

func (s *FileStore) locked(fn func(map[string]Registration) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	regs := map[string]Registration{}
	switch b, err := os.ReadFile(s.Path); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, &regs); err != nil {
			return fmt.Errorf("%s: %w", s.Path, err)
		}
	}
	write, err := fn(regs)
	if err != nil || !write {
		return err
	}
	b, err := json.MarshalIndent(regs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".wazuh-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

func (s *FileStore) Get(_ context.Context, asset string) (r Registration, ok bool, err error) {
	err = s.locked(func(regs map[string]Registration) (bool, error) {
		r, ok = regs[asset]
		return false, nil
	})
	return r, ok, err
}

func (s *FileStore) Put(_ context.Context, r Registration, prev int64) error {
	return s.locked(func(regs map[string]Registration) (bool, error) {
		if regs[r.Asset].Generation != prev || r.Generation != prev+1 {
			return false, ErrConflict
		}
		regs[r.Asset] = r
		return true, nil
	})
}

func (s *FileStore) Delete(_ context.Context, asset string, prev int64) error {
	return s.locked(func(regs map[string]Registration) (bool, error) {
		if regs[asset].Generation != prev {
			return false, ErrConflict
		}
		delete(regs, asset)
		return true, nil
	})
}

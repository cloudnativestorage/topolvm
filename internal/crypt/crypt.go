package crypt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Defaults match the spec recommendations (AES-256-XTS, Argon2id).
const (
	DefaultCipher  = "aes-xts-plain64"
	DefaultKeySize = 512
	DefaultPBKDF   = "argon2id"
)

// FormatOpts controls the LUKS2 header layout.
type FormatOpts struct {
	Cipher  string
	KeySize int
	PBKDF   string
	// Integrity selects an authenticated-encryption mode. Empty (default)
	// leaves integrity off. "hmac-sha256" passes --integrity hmac-sha256
	// to luksFormat so every sector carries an HMAC tag.
	Integrity string
	// NoWipe skips the initial integrity wipe. Defaults to false (full
	// wipe). With NoWipe=true, reads of never-written sectors fail
	// authentication until written; mkfs initializes most of the device.
	NoWipe bool
}

// WithDefaults fills in any unset fields with package defaults.
func (o FormatOpts) WithDefaults() FormatOpts {
	if o.Cipher == "" {
		o.Cipher = DefaultCipher
	}
	if o.KeySize == 0 {
		o.KeySize = DefaultKeySize
	}
	if o.PBKDF == "" {
		o.PBKDF = DefaultPBKDF
	}
	return o
}

// IntegrityHMACSHA256 is the only integrity profile we currently expose; it
// maps to cryptsetup's --integrity hmac-sha256.
const IntegrityHMACSHA256 = "hmac-sha256"

// ReencryptOpts controls online master-key reencryption.
type ReencryptOpts struct {
	// Cipher is the target cipher; empty preserves the current cipher.
	Cipher string
	// Resilience is the cryptsetup --resilience mode; defaults to "checksum"
	// so the operation is restartable after a crash or node reboot.
	Resilience string
	// HotzoneSize is the cryptsetup --hotzone-size argument used as a
	// rudimentary throughput limiter. Empty leaves the default.
	HotzoneSize string
}

// HeaderProfile describes the on-disk LUKS2 header that an open call would
// see. Returned by HeaderProfile() and used by the node-side mismatch guard.
type HeaderProfile struct {
	Cipher    string
	Integrity string // "" when integrity is not enabled
}

// Manager is the surface the node code uses to drive cryptsetup. The exec.go
// implementation shells out to cryptsetup; tests inject a fake CmdRunner.
type Manager interface {
	IsLuks(ctx context.Context, device string) (bool, error)
	Format(ctx context.Context, device string, pass SecretBuf, opts FormatOpts) error
	Open(ctx context.Context, device, name string, pass SecretBuf) (mapper string, err error)
	Close(ctx context.Context, name string) error
	IsOpen(ctx context.Context, name string) (bool, error)
	Resize(ctx context.Context, name string, pass SecretBuf) error
	AddKey(ctx context.Context, device string, oldPass, newPass SecretBuf) (slot int, err error)
	KillSlot(ctx context.Context, device string, slot int, pass SecretBuf) error
	Reencrypt(ctx context.Context, device string, pass SecretBuf, opts ReencryptOpts) error
	HeaderUUID(ctx context.Context, device string) (string, error)
	HeaderProfile(ctx context.Context, device string) (HeaderProfile, error)
	// IntegritySupported reports whether the running cryptsetup binary and
	// kernel can format / open LUKS2-with-integrity devices. The node uses
	// this to fail loudly when a StorageClass requests integrity on an
	// unsupported node instead of silently falling back.
	IntegritySupported(ctx context.Context) (bool, error)
}

// CmdResult captures everything the wrapper needs from a cryptsetup invocation.
type CmdResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// CmdRunner is the seam tests use to assert exact argv and stdin without
// invoking real cryptsetup. extraFD is non-nil when the caller passed a second
// secret stream (used by luksAddKey).
type CmdRunner interface {
	Run(ctx context.Context, name string, args []string, stdin, extraFD io.Reader) (CmdResult, error)
}

// MapperPathPrefix is /dev/mapper, where cryptsetup creates the plaintext view.
const MapperPathPrefix = "/dev/mapper/"

// MapperPath joins MapperPathPrefix with the device-mapper name.
func MapperPath(name string) string {
	return MapperPathPrefix + name
}

// execManager is the production cryptsetup-backed Manager.
type execManager struct {
	binary string
	runner CmdRunner
}

// NewManager returns a Manager that shells out to cryptsetup. binary defaults
// to "cryptsetup" when empty.
func NewManager(binary string) Manager {
	if binary == "" {
		binary = "cryptsetup"
	}
	return &execManager{binary: binary, runner: defaultRunner{}}
}

// NewManagerWithRunner is used by tests to inject a fake CmdRunner.
func NewManagerWithRunner(binary string, runner CmdRunner) Manager {
	if binary == "" {
		binary = "cryptsetup"
	}
	if runner == nil {
		runner = defaultRunner{}
	}
	return &execManager{binary: binary, runner: runner}
}

// IsLuks returns true if device has a LUKS header.
func (m *execManager) IsLuks(ctx context.Context, device string) (bool, error) {
	res, err := m.runner.Run(ctx, m.binary, []string{"isLuks", device}, nil, nil)
	if err == nil {
		return true, nil
	}
	if res.ExitCode > 0 {
		// Exit code 1 from isLuks means "not a LUKS device" rather than
		// an operational failure; do not surface as an error.
		return false, nil
	}
	return false, fmt.Errorf("crypt: isLuks %s: %w", device, err)
}

// Format runs luksFormat with the passphrase passed only on stdin.
func (m *execManager) Format(ctx context.Context, device string, pass SecretBuf, opts FormatOpts) error {
	if pass == nil || pass.Len() == 0 {
		return errors.New("crypt: empty passphrase for luksFormat")
	}
	opts = opts.WithDefaults()
	args := []string{
		"luksFormat",
		"--type", "luks2",
		"--cipher", opts.Cipher,
		"--key-size", fmt.Sprintf("%d", opts.KeySize),
		"--pbkdf", opts.PBKDF,
		"--batch-mode",
	}
	if opts.Integrity != "" {
		args = append(args, "--integrity", opts.Integrity)
		if opts.NoWipe {
			args = append(args, "--integrity-no-wipe")
		}
	}
	args = append(args, device, "--key-file=-")
	if err := m.runWithSecret(ctx, args, pass); err != nil {
		return fmt.Errorf("crypt: luksFormat %s: %w", device, err)
	}
	return nil
}

// Open unlocks device and exposes it at /dev/mapper/<name>.
func (m *execManager) Open(ctx context.Context, device, name string, pass SecretBuf) (string, error) {
	if pass == nil || pass.Len() == 0 {
		return "", errors.New("crypt: empty passphrase for luksOpen")
	}
	args := []string{"open", "--type", "luks2", device, name, "--key-file=-"}
	if err := m.runWithSecret(ctx, args, pass); err != nil {
		return "", fmt.Errorf("crypt: open %s as %s: %w", device, name, err)
	}
	return MapperPath(name), nil
}

// Close closes the mapper, dropping the master key from the kernel keyring.
func (m *execManager) Close(ctx context.Context, name string) error {
	_, err := m.runner.Run(ctx, m.binary, []string{"close", name}, nil, nil)
	if err != nil {
		return fmt.Errorf("crypt: close %s: %w", name, err)
	}
	return nil
}

// IsOpen returns true when /dev/mapper/<name> is an active dm-crypt mapping.
func (m *execManager) IsOpen(ctx context.Context, name string) (bool, error) {
	res, err := m.runner.Run(ctx, m.binary, []string{"status", name}, nil, nil)
	if err == nil {
		return true, nil
	}
	if res.ExitCode > 0 {
		return false, nil
	}
	return false, fmt.Errorf("crypt: status %s: %w", name, err)
}

// Resize grows the dm-crypt mapping after the ciphertext LV has been extended.
func (m *execManager) Resize(ctx context.Context, name string, pass SecretBuf) error {
	if pass == nil || pass.Len() == 0 {
		return errors.New("crypt: empty passphrase for resize")
	}
	args := []string{"resize", name, "--key-file=-"}
	if err := m.runWithSecret(ctx, args, pass); err != nil {
		return fmt.Errorf("crypt: resize %s: %w", name, err)
	}
	return nil
}

// AddKey adds a new passphrase to a free LUKS2 keyslot. The existing
// passphrase is supplied on stdin; the new passphrase is supplied via
// /dev/fd/3 from a pipe so it never appears in argv or on disk.
func (m *execManager) AddKey(ctx context.Context, device string, oldPass, newPass SecretBuf) (int, error) {
	if oldPass == nil || oldPass.Len() == 0 {
		return -1, errors.New("crypt: empty existing passphrase for luksAddKey")
	}
	if newPass == nil || newPass.Len() == 0 {
		return -1, errors.New("crypt: empty new passphrase for luksAddKey")
	}
	args := []string{
		"luksAddKey",
		"--batch-mode",
		device,
		"--key-file=-",
		"--new-keyfile=/dev/fd/3",
	}
	stdin := bytes.NewReader(oldPass.Bytes())
	extra := bytes.NewReader(newPass.Bytes())
	res, err := m.runner.Run(ctx, m.binary, args, stdin, extra)
	if err != nil {
		return -1, fmt.Errorf("crypt: luksAddKey %s: %w: %s", device, err, strings.TrimSpace(string(res.Stderr)))
	}
	slot, err := m.lookupNewestSlot(ctx, device)
	if err != nil {
		return -1, fmt.Errorf("crypt: lookup new slot on %s: %w", device, err)
	}
	return slot, nil
}

// KillSlot removes a passphrase keyslot. pass authenticates with any other
// surviving slot. May be nil only if cryptsetup is running with
// --batch-mode and the slot is being killed unauthenticated, which we do not
// allow here.
func (m *execManager) KillSlot(ctx context.Context, device string, slot int, pass SecretBuf) error {
	if pass == nil || pass.Len() == 0 {
		return errors.New("crypt: empty passphrase for luksKillSlot")
	}
	args := []string{"luksKillSlot", "--batch-mode", device, fmt.Sprintf("%d", slot), "--key-file=-"}
	if err := m.runWithSecret(ctx, args, pass); err != nil {
		return fmt.Errorf("crypt: luksKillSlot %s/%d: %w", device, slot, err)
	}
	return nil
}

// Reencrypt drives an online master-key rotation. --resilience checksum makes
// the operation restartable; --batch-mode avoids interactive prompts.
func (m *execManager) Reencrypt(ctx context.Context, device string, pass SecretBuf, opts ReencryptOpts) error {
	if pass == nil || pass.Len() == 0 {
		return errors.New("crypt: empty passphrase for reencrypt")
	}
	resilience := opts.Resilience
	if resilience == "" {
		resilience = "checksum"
	}
	args := []string{
		"reencrypt",
		"--resilience", resilience,
		"--batch-mode",
		device,
		"--key-file=-",
	}
	if opts.Cipher != "" {
		args = append(args, "--cipher", opts.Cipher)
	}
	if opts.HotzoneSize != "" {
		args = append(args, "--hotzone-size", opts.HotzoneSize)
	}
	if err := m.runWithSecret(ctx, args, pass); err != nil {
		return fmt.Errorf("crypt: reencrypt %s: %w", device, err)
	}
	return nil
}

// HeaderUUID returns the LUKS2 header UUID.
func (m *execManager) HeaderUUID(ctx context.Context, device string) (string, error) {
	res, err := m.runner.Run(ctx, m.binary, []string{"luksUUID", device}, nil, nil)
	if err != nil {
		return "", fmt.Errorf("crypt: luksUUID %s: %w", device, err)
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// HeaderProfile reports the cipher and integrity profile of the on-disk
// header. Used by the node to fail mismatched StorageClass / on-disk state.
func (m *execManager) HeaderProfile(ctx context.Context, device string) (HeaderProfile, error) {
	res, err := m.runner.Run(ctx, m.binary, []string{"luksDump", device}, nil, nil)
	if err != nil {
		return HeaderProfile{}, fmt.Errorf("crypt: luksDump %s: %w", device, err)
	}
	hp := HeaderProfile{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Cipher:"):
			hp.Cipher = strings.TrimSpace(strings.TrimPrefix(t, "Cipher:"))
		case strings.HasPrefix(t, "Integrity:"):
			v := strings.TrimSpace(strings.TrimPrefix(t, "Integrity:"))
			// Older cryptsetup prints "(no)" when integrity is disabled.
			if v != "(no)" && v != "" {
				hp.Integrity = v
			}
		}
	}
	return hp, nil
}

// IntegritySupported runs `cryptsetup --version` plus a simple cryptsetup
// help probe. We treat any output mentioning "integrity" in the binary's
// help / version output as a positive indicator. If the binary cannot be
// invoked at all, we surface that as the error.
func (m *execManager) IntegritySupported(ctx context.Context) (bool, error) {
	res, err := m.runner.Run(ctx, m.binary, []string{"--version"}, nil, nil)
	if err != nil {
		return false, fmt.Errorf("crypt: cryptsetup --version: %w", err)
	}
	out := strings.ToLower(string(res.Stdout) + string(res.Stderr))
	if strings.Contains(out, "cryptsetup") {
		// Probe help for the --integrity flag.
		help, _ := m.runner.Run(ctx, m.binary, []string{"luksFormat", "--help"}, nil, nil)
		if strings.Contains(strings.ToLower(string(help.Stdout)+string(help.Stderr)), "--integrity") {
			return true, nil
		}
	}
	return false, nil
}

// runWithSecret pipes a SecretBuf to stdin without copying it into argv/env.
func (m *execManager) runWithSecret(ctx context.Context, args []string, pass SecretBuf) error {
	stdin := bytes.NewReader(pass.Bytes())
	res, err := m.runner.Run(ctx, m.binary, args, stdin, nil)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// lookupNewestSlot returns the highest occupied LUKS2 keyslot index.
func (m *execManager) lookupNewestSlot(ctx context.Context, device string) (int, error) {
	res, err := m.runner.Run(ctx, m.binary, []string{"luksDump", device}, nil, nil)
	if err != nil {
		return -1, err
	}
	slot := -1
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		var idx int
		if _, scanErr := fmt.Sscanf(line, "%d: luks2", &idx); scanErr == nil {
			if idx > slot {
				slot = idx
			}
		}
	}
	if slot < 0 {
		return -1, errors.New("crypt: no occupied keyslots reported by luksDump")
	}
	return slot, nil
}

// defaultRunner shells out via os/exec. The extra-FD machinery is used by
// luksAddKey so the new passphrase never appears in argv or on disk.
type defaultRunner struct{}

func (defaultRunner) Run(ctx context.Context, name string, args []string, stdin, extraFD io.Reader) (CmdResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	var extraWriter *os.File
	if extraFD != nil {
		pr, pw, err := os.Pipe()
		if err != nil {
			return CmdResult{}, err
		}
		cmd.ExtraFiles = []*os.File{pr}
		extraWriter = pw
		defer func() { _ = pr.Close() }()
	}

	if err := cmd.Start(); err != nil {
		if extraWriter != nil {
			_ = extraWriter.Close()
		}
		return CmdResult{Stderr: stderr.Bytes()}, err
	}

	if extraWriter != nil {
		go func() {
			defer func() { _ = extraWriter.Close() }()
			_, _ = io.Copy(extraWriter, extraFD)
		}()
	}

	waitErr := cmd.Wait()
	code := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
		}
	}
	return CmdResult{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		ExitCode: code,
	}, waitErr
}

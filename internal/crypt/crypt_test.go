package crypt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// recordingRunner captures every invocation so tests can assert exact argv,
// stdin contents, and absence of secret material in argv.
type recordingRunner struct {
	calls []recordedCall
	// queued results, popped FIFO. If empty, returns the zero CmdResult.
	queue []recordedReply
}

type recordedCall struct {
	Name      string
	Args      []string
	Stdin     []byte
	ExtraFD   []byte
	HasStdin  bool
	HasExtra  bool
	StdinPath string // set when stdin was read via path style
}

type recordedReply struct {
	Result CmdResult
	Err    error
}

func (r *recordingRunner) Run(ctx context.Context, name string, args []string, stdin, extraFD io.Reader) (CmdResult, error) {
	call := recordedCall{Name: name, Args: append([]string(nil), args...)}
	if stdin != nil {
		buf, _ := io.ReadAll(stdin)
		call.Stdin = buf
		call.HasStdin = true
	}
	if extraFD != nil {
		buf, _ := io.ReadAll(extraFD)
		call.ExtraFD = buf
		call.HasExtra = true
	}
	r.calls = append(r.calls, call)
	if len(r.queue) == 0 {
		return CmdResult{}, nil
	}
	reply := r.queue[0]
	r.queue = r.queue[1:]
	return reply.Result, reply.Err
}

func (r *recordingRunner) push(res CmdResult, err error) {
	r.queue = append(r.queue, recordedReply{Result: res, Err: err})
}

func mustSecret(t *testing.T, payload string) SecretBuf {
	t.Helper()
	b, err := SecretBufFrom([]byte(payload))
	if err != nil {
		t.Fatalf("SecretBufFrom: %v", err)
	}
	return b
}

// assertNoSecretInArgv guarantees the test passphrase never appears in argv.
func assertNoSecretInArgv(t *testing.T, args []string, secret string) {
	t.Helper()
	for _, a := range args {
		if strings.Contains(a, secret) {
			t.Fatalf("secret leaked into argv: arg=%q", a)
		}
	}
}

func TestIsLuks_Yes(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{}, nil)
	m := NewManagerWithRunner("cryptsetup", r)
	ok, err := m.IsLuks(context.Background(), "/dev/topolvm/abc")
	if err != nil || !ok {
		t.Fatalf("IsLuks: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(r.calls[0].Args, []string{"isLuks", "/dev/topolvm/abc"}) {
		t.Fatalf("unexpected argv: %v", r.calls[0].Args)
	}
}

func TestIsLuks_No(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{ExitCode: 1}, errors.New("exit 1"))
	m := NewManagerWithRunner("", r)
	ok, err := m.IsLuks(context.Background(), "/dev/topolvm/abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false on non-LUKS device")
	}
}

func TestFormat_ArgvAndStdin(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{}, nil)
	m := NewManagerWithRunner("", r)
	pass := mustSecret(t, "s3cretpass")
	defer pass.Destroy()
	if err := m.Format(context.Background(), "/dev/topolvm/abc", pass, FormatOpts{}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	call := r.calls[0]
	want := []string{
		"luksFormat",
		"--type", "luks2",
		"--cipher", "aes-xts-plain64",
		"--key-size", "512",
		"--pbkdf", "argon2id",
		"--batch-mode",
		"/dev/topolvm/abc",
		"--key-file=-",
	}
	if !reflect.DeepEqual(call.Args, want) {
		t.Fatalf("argv mismatch\n got %v\nwant %v", call.Args, want)
	}
	assertNoSecretInArgv(t, call.Args, "s3cretpass")
	if !call.HasStdin {
		t.Fatal("expected stdin to carry the passphrase")
	}
	if !bytes.Equal(call.Stdin, []byte("s3cretpass")) {
		t.Fatalf("stdin mismatch: %q", call.Stdin)
	}
}

func TestFormat_WithIntegrity(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{}, nil)
	m := NewManagerWithRunner("", r)
	pass := mustSecret(t, "pw")
	defer pass.Destroy()
	if err := m.Format(context.Background(), "/dev/topolvm/abc", pass, FormatOpts{Integrity: IntegrityHMACSHA256, NoWipe: true}); err != nil {
		t.Fatalf("Format: %v", err)
	}
	call := r.calls[0]
	want := []string{
		"luksFormat",
		"--type", "luks2",
		"--cipher", "aes-xts-plain64",
		"--key-size", "512",
		"--pbkdf", "argon2id",
		"--batch-mode",
		"--integrity", "hmac-sha256",
		"--integrity-no-wipe",
		"/dev/topolvm/abc",
		"--key-file=-",
	}
	if !reflect.DeepEqual(call.Args, want) {
		t.Fatalf("argv mismatch\n got %v\nwant %v", call.Args, want)
	}
}

func TestHeaderProfile_ParsesIntegrity(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{Stdout: []byte("LUKS header information\nVersion: 2\nCipher: aes-xts-plain64\nIntegrity: hmac-sha256\nKeyslots:\n  0: luks2\n")}, nil)
	m := NewManagerWithRunner("", r)
	hp, err := m.HeaderProfile(context.Background(), "/dev/topolvm/abc")
	if err != nil {
		t.Fatalf("HeaderProfile: %v", err)
	}
	if hp.Cipher != "aes-xts-plain64" || hp.Integrity != "hmac-sha256" {
		t.Fatalf("unexpected profile: %+v", hp)
	}
}

func TestHeaderProfile_NoIntegrity(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{Stdout: []byte("Cipher: aes-xts-plain64\nIntegrity: (no)\n")}, nil)
	m := NewManagerWithRunner("", r)
	hp, _ := m.HeaderProfile(context.Background(), "x")
	if hp.Integrity != "" {
		t.Fatalf("expected empty integrity, got %q", hp.Integrity)
	}
}

func TestIntegritySupported_True(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{Stdout: []byte("cryptsetup 2.7.0\n")}, nil)
	r.push(CmdResult{Stdout: []byte("usage: cryptsetup luksFormat ... --integrity <hash>\n")}, nil)
	m := NewManagerWithRunner("", r)
	ok, err := m.IntegritySupported(context.Background())
	if err != nil {
		t.Fatalf("IntegritySupported: %v", err)
	}
	if !ok {
		t.Fatal("expected supported=true")
	}
}

func TestIntegritySupported_False(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{Stdout: []byte("cryptsetup 1.7.0\n")}, nil)
	r.push(CmdResult{Stdout: []byte("usage: cryptsetup luksFormat ... (no integrity here)\n")}, nil)
	m := NewManagerWithRunner("", r)
	ok, _ := m.IntegritySupported(context.Background())
	if ok {
		t.Fatal("expected supported=false")
	}
}

func TestFormat_RejectsEmptyPass(t *testing.T) {
	r := &recordingRunner{}
	m := NewManagerWithRunner("", r)
	err := m.Format(context.Background(), "/dev/topolvm/abc", nil, FormatOpts{})
	if err == nil {
		t.Fatal("expected error for nil pass")
	}
	if len(r.calls) != 0 {
		t.Fatal("must not call cryptsetup with empty passphrase")
	}
}

func TestOpen_ReturnsMapperPath(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{}, nil)
	m := NewManagerWithRunner("", r)
	pass := mustSecret(t, "pw")
	defer pass.Destroy()
	mp, err := m.Open(context.Background(), "/dev/topolvm/abc", "topolvm-abc", pass)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if mp != "/dev/mapper/topolvm-abc" {
		t.Fatalf("mapper path: %q", mp)
	}
	call := r.calls[0]
	want := []string{"open", "--type", "luks2", "/dev/topolvm/abc", "topolvm-abc", "--key-file=-"}
	if !reflect.DeepEqual(call.Args, want) {
		t.Fatalf("argv mismatch\n got %v\nwant %v", call.Args, want)
	}
	assertNoSecretInArgv(t, call.Args, "pw")
}

func TestClose_NoSecret(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{}, nil)
	m := NewManagerWithRunner("", r)
	if err := m.Close(context.Background(), "topolvm-abc"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !reflect.DeepEqual(r.calls[0].Args, []string{"close", "topolvm-abc"}) {
		t.Fatalf("argv mismatch: %v", r.calls[0].Args)
	}
	if r.calls[0].HasStdin {
		t.Fatal("Close must not write a secret to stdin")
	}
}

func TestResize_PassphraseViaStdin(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{}, nil)
	m := NewManagerWithRunner("", r)
	pass := mustSecret(t, "pw")
	defer pass.Destroy()
	if err := m.Resize(context.Background(), "topolvm-abc", pass); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	want := []string{"resize", "topolvm-abc", "--key-file=-"}
	if !reflect.DeepEqual(r.calls[0].Args, want) {
		t.Fatalf("argv mismatch: %v", r.calls[0].Args)
	}
	if !bytes.Equal(r.calls[0].Stdin, []byte("pw")) {
		t.Fatalf("stdin mismatch: %q", r.calls[0].Stdin)
	}
}

func TestAddKey_ExtraFDAndDump(t *testing.T) {
	r := &recordingRunner{}
	// First call: luksAddKey - succeed.
	r.push(CmdResult{}, nil)
	// Second call: luksDump - return one occupied slot at index 1.
	r.push(CmdResult{Stdout: []byte("Keyslots:\n  0: luks2\n  1: luks2\n")}, nil)
	m := NewManagerWithRunner("", r)
	oldPass := mustSecret(t, "passphrase-1")
	defer oldPass.Destroy()
	newPass := mustSecret(t, "passphrase-2")
	defer newPass.Destroy()
	slot, err := m.AddKey(context.Background(), "/dev/topolvm/abc", oldPass, newPass)
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if slot != 1 {
		t.Fatalf("slot = %d, want 1", slot)
	}
	addCall := r.calls[0]
	want := []string{
		"luksAddKey",
		"--batch-mode",
		"/dev/topolvm/abc",
		"--key-file=-",
		"--new-keyfile=/dev/fd/3",
	}
	if !reflect.DeepEqual(addCall.Args, want) {
		t.Fatalf("argv mismatch\n got %v\nwant %v", addCall.Args, want)
	}
	assertNoSecretInArgv(t, addCall.Args, "passphrase-1")
	assertNoSecretInArgv(t, addCall.Args, "passphrase-2")
	if !bytes.Equal(addCall.Stdin, []byte("passphrase-1")) {
		t.Fatalf("stdin mismatch: %q", addCall.Stdin)
	}
	if !bytes.Equal(addCall.ExtraFD, []byte("passphrase-2")) {
		t.Fatalf("extraFD mismatch: %q", addCall.ExtraFD)
	}
}

func TestKillSlot_ArgvAndStdin(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{}, nil)
	m := NewManagerWithRunner("", r)
	pass := mustSecret(t, "pw")
	defer pass.Destroy()
	if err := m.KillSlot(context.Background(), "/dev/topolvm/abc", 3, pass); err != nil {
		t.Fatalf("KillSlot: %v", err)
	}
	want := []string{"luksKillSlot", "--batch-mode", "/dev/topolvm/abc", "3", "--key-file=-"}
	if !reflect.DeepEqual(r.calls[0].Args, want) {
		t.Fatalf("argv mismatch: %v", r.calls[0].Args)
	}
	if !bytes.Equal(r.calls[0].Stdin, []byte("pw")) {
		t.Fatalf("stdin mismatch: %q", r.calls[0].Stdin)
	}
}

func TestReencrypt_DefaultsAndOverrides(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{}, nil)
	m := NewManagerWithRunner("", r)
	pass := mustSecret(t, "pw")
	defer pass.Destroy()
	err := m.Reencrypt(context.Background(), "/dev/topolvm/abc", pass, ReencryptOpts{
		Cipher:      "aes-xts-plain64",
		HotzoneSize: "16M",
	})
	if err != nil {
		t.Fatalf("Reencrypt: %v", err)
	}
	want := []string{
		"reencrypt",
		"--resilience", "checksum",
		"--batch-mode",
		"/dev/topolvm/abc",
		"--key-file=-",
		"--cipher", "aes-xts-plain64",
		"--hotzone-size", "16M",
	}
	if !reflect.DeepEqual(r.calls[0].Args, want) {
		t.Fatalf("argv mismatch\n got %v\nwant %v", r.calls[0].Args, want)
	}
	assertNoSecretInArgv(t, r.calls[0].Args, "pw")
}

func TestHeaderUUID(t *testing.T) {
	r := &recordingRunner{}
	r.push(CmdResult{Stdout: []byte("a1b2c3d4-e5f6-7890-1234-56789abcdef0\n")}, nil)
	m := NewManagerWithRunner("", r)
	u, err := m.HeaderUUID(context.Background(), "/dev/topolvm/abc")
	if err != nil {
		t.Fatalf("HeaderUUID: %v", err)
	}
	if u != "a1b2c3d4-e5f6-7890-1234-56789abcdef0" {
		t.Fatalf("uuid: %q", u)
	}
}

func TestSecretBuf_DestroyZeroizes(t *testing.T) {
	src := []byte("topsecret")
	b, err := SecretBufFrom(src)
	if err != nil {
		t.Fatalf("SecretBufFrom: %v", err)
	}
	// src must already be zeroized by SecretBufFrom.
	for i, c := range src {
		if c != 0 {
			t.Fatalf("source byte %d not zeroized: %d", i, c)
		}
	}
	// snapshot the destination, then destroy and ensure it is zero.
	got := append([]byte(nil), b.Bytes()...)
	if string(got) != "topsecret" {
		t.Fatalf("buf contents: %q", got)
	}
	b.Destroy()
	if b.Bytes() != nil {
		t.Fatalf("Bytes() after Destroy must be nil, got %v", b.Bytes())
	}
}

func TestMapperPath(t *testing.T) {
	if got := MapperPath("topolvm-abc"); got != "/dev/mapper/topolvm-abc" {
		t.Fatalf("MapperPath: %q", got)
	}
}

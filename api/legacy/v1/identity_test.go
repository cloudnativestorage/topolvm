package v1_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	legacyv1 "github.com/topolvm/topolvm/api/legacy/v1"
	topolvmv1 "github.com/topolvm/topolvm/api/v1"
)

// The api/v1 and api/legacy/v1 packages are intentional copies that serve two
// different group names (topolvm.io vs topolvm.cybozu.com). Whenever a type or
// const is added to one, the same change must be made to the other or the
// legacy CRD silently drops the field. These tests fail fast if the two trees
// drift, so the regression is caught at unit-test time instead of at runtime
// in a cluster that's still using the legacy group.

// typePairs lists every Snapshot/Backup-Storage type that exists in both trees
// and must stay structurally identical. Add new pairs here when introducing a
// new shared type.
var typePairs = []struct {
	name   string
	new    any
	legacy any
}{
	{"SnapshotBackupStorage", topolvmv1.SnapshotBackupStorage{}, legacyv1.SnapshotBackupStorage{}},
	{"SnapshotBackupStorageSpec", topolvmv1.SnapshotBackupStorageSpec{}, legacyv1.SnapshotBackupStorageSpec{}},
	{"SnapshotBackupStorageSpecStatus", topolvmv1.SnapshotBackupStorageSpecStatus{}, legacyv1.SnapshotBackupStorageSpecStatus{}},
	{"SnapshotBackupStorageList", topolvmv1.SnapshotBackupStorageList{}, legacyv1.SnapshotBackupStorageList{}},
	{"Storage", topolvmv1.Storage{}, legacyv1.Storage{}},
	{"S3Spec", topolvmv1.S3Spec{}, legacyv1.S3Spec{}},
	{"GCSSpec", topolvmv1.GCSSpec{}, legacyv1.GCSSpec{}},
	{"AzureSpec", topolvmv1.AzureSpec{}, legacyv1.AzureSpec{}},
}

func TestSharedTypesAreStructurallyIdentical(t *testing.T) {
	for _, p := range typePairs {
		t.Run(p.name, func(t *testing.T) {
			compareStructs(t, p.name, reflect.TypeOf(p.new), reflect.TypeOf(p.legacy))
		})
	}
}

// compareStructs asserts that two struct types have the same exported fields,
// in the same order, with the same names, JSON tags, and Kinds. It does not
// compare the full package-qualified type names because the two trees
// legitimately reference *different* packages for the same logical type
// (e.g. v1.S3Spec vs legacyv1.S3Spec). For struct-kind fields, it recurses.
func compareStructs(t *testing.T, path string, a, b reflect.Type) {
	t.Helper()
	if a.Kind() != reflect.Struct || b.Kind() != reflect.Struct {
		t.Fatalf("%s: expected structs, got %s vs %s", path, a.Kind(), b.Kind())
	}
	if a.NumField() != b.NumField() {
		t.Fatalf("%s: field count drift: api/v1 has %d, api/legacy/v1 has %d", path, a.NumField(), b.NumField())
	}
	for i := 0; i < a.NumField(); i++ {
		fa, fb := a.Field(i), b.Field(i)
		fieldPath := path + "." + fa.Name
		if fa.Name != fb.Name {
			t.Errorf("%s: field name mismatch at index %d: %q vs %q", path, i, fa.Name, fb.Name)
			continue
		}
		if fa.Tag.Get("json") != fb.Tag.Get("json") {
			t.Errorf("%s: json tag mismatch: %q vs %q", fieldPath, fa.Tag.Get("json"), fb.Tag.Get("json"))
		}
		if fa.Type.Kind() != fb.Type.Kind() {
			t.Errorf("%s: kind mismatch: %s vs %s", fieldPath, fa.Type.Kind(), fb.Type.Kind())
			continue
		}
		// Recurse for embedded structs in the same module. Cross-package
		// shared types (e.g. metav1.TypeMeta) have identical reflect.Type
		// values in both trees, so the recursion is a no-op there.
		if fa.Type.Kind() == reflect.Struct && fa.Type.PkgPath() != "" {
			compareStructs(t, fieldPath, fa.Type, fb.Type)
		}
	}
}

func TestSharedExportedConstsMatch(t *testing.T) {
	apiDir := apiPackageDir(t)
	newConsts := exportedConstNames(t, filepath.Join(apiDir, "v1", "constants.go"))
	legacyConsts := exportedConstNames(t, filepath.Join(apiDir, "legacy", "v1", "constants.go"))

	onlyNew := diffSorted(newConsts, legacyConsts)
	onlyLegacy := diffSorted(legacyConsts, newConsts)

	if len(onlyNew) > 0 {
		t.Errorf("constants present in api/v1 but missing from api/legacy/v1: %v", onlyNew)
	}
	if len(onlyLegacy) > 0 {
		t.Errorf("constants present in api/legacy/v1 but missing from api/v1: %v", onlyLegacy)
	}
}

func exportedConstNames(t *testing.T, path string) map[string]struct{} {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("failed to parse %s: %v", path, err)
	}
	names := map[string]struct{}{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				if n.IsExported() {
					names[n.Name] = struct{}{}
				}
			}
		}
	}
	return names
}

func diffSorted(a, b map[string]struct{}) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// apiPackageDir returns the absolute path to the api/ directory, derived from
// this test file's own location so it works regardless of how `go test` is
// invoked.
func apiPackageDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = .../api/legacy/v1/identity_test.go
	dir := filepath.Dir(thisFile)         // .../api/legacy/v1
	dir = filepath.Dir(filepath.Dir(dir)) // .../api
	if _, err := os.Stat(filepath.Join(dir, "v1", "constants.go")); err != nil {
		t.Fatalf("could not locate api/v1/constants.go from %s: %v", dir, err)
	}
	return dir
}

// Package buildinfo reports which build a running process came from, so a
// deployment can be verified from outside.
//
// The obvious answer — the commit SHA — is not available here. The build
// context excludes `.git`, so nothing in the image knows the commit, and
// Easypanel has no separate build-argument field: build arguments come from a
// service's environment, which is static per service and therefore cannot name
// a commit that only exists after a push.
//
// What does change with every commit is the source itself. The Dockerfile
// hashes every Go file of the backend and the shared module into Fingerprint
// and bakes it into the binary with -ldflags. A caller who reads the
// fingerprint from a live process and computes the same hash from a checkout
// can prove the deployed image is that tree — without a panel, a shell, or a
// commit SHA.
package buildinfo

// Set at build time with `-ldflags "-X ..."`. They stay "unknown" for a build
// that passes no flag, such as `go run ./cmd/api` or a plain `go build`.
var (
	// Commit is the git revision the image was built from, when the build was
	// told it.
	Commit = "unknown"
	// BuiltAt is the build timestamp, when the build was told it.
	BuiltAt = "unknown"
	// Fingerprint is the truncated SHA-256 of every Go file in the build, which
	// is the only deploy identity the build can compute by itself.
	Fingerprint = "unknown"
)

// Info is the build identity in the shape the health endpoint publishes.
type Info struct {
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at"`
	Build   string `json:"build"`
}

// Snapshot returns the build identity of this process. A value the build did
// not set is reported as "unknown" rather than as an empty string, so a probe
// always answers the same shape and an absent field is not mistaken for a
// broken one.
func Snapshot() Info {
	return Info{
		Commit:  orUnknown(Commit),
		BuiltAt: orUnknown(BuiltAt),
		Build:   orUnknown(Fingerprint),
	}
}

func orUnknown(value string) string {
	if value == "" {
		return "unknown"
	}

	return value
}

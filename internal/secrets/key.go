package secrets

import "strings"

// LocalKey is the canonical on-disk layout used by every provider:
//
//	<name>-<env>/<key>
//
// File-SoT providers (Hetzner, libvirt) read and write this directly. The
// AWS Cached layer translates its SSM-shaped remote keys into this same shape
// before caching, so apps and operators find their kubeconfig in the same
// place under $BONSAI_DATA_DIR regardless of provider.
func LocalKey(name, env, key string) string {
	return name + "-" + env + "/" + key
}

// SSMToLocal turns an SSM parameter path of the form "/bonsai/<name>/<env>/<key>"
// into the canonical local-cache key "<name>-<env>/<key>". Falls back to the
// input verbatim if the shape does not match — the cache layer should never
// fail a write because of an unfamiliar key.
func SSMToLocal(ssmPath string) string {
	s := strings.TrimPrefix(ssmPath, "/bonsai/")
	if s == ssmPath {
		return ssmPath
	}
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 {
		return ssmPath
	}
	return parts[0] + "-" + parts[1] + "/" + parts[2]
}

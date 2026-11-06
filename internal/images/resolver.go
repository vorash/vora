package images

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ctrclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/opencontainers/image-spec/identity"

	"github.com/vorash/vora/internal/config"
	vctr "github.com/vorash/vora/internal/containerd"
)

// containerdRetryAfter is how long a ref the local cache served stays out of
// containerd's reach. A pull of an image no registry has spends seconds in retries,
// on every start of every sandbox, so it can't be tried each time. It is not forever,
// because the same ref can be pulled into containerd later.
const containerdRetryAfter = 5 * time.Minute

// Resolver resolves an image reference to a read-only rootfs directory. It
// prefers the containerd image store (ADR 009) and falls back to the local
// Docker-export cache when containerd is unavailable or the ref is not stored
// there (unless the mode pins one source).
type Resolver struct {
	cache       *Cache
	mode        string
	snapshotter string
	dataRoot    string
	cli         *ctrclient.Client
	logger      *slog.Logger
	policy      *Policy
	policyErr   error

	// fromContainerd is containerdRootfs when a client is connected.
	fromContainerd func(ctx context.Context, ref string) (string, error)

	mu   sync.Mutex
	refs map[string]string
	// cacheOnly holds when each ref the cache served after containerd failed may try
	// containerd again.
	cacheOnly map[string]time.Time
}

// NewResolver builds a resolver. It never fails: when containerd is
// unavailable it resolves through the local rootfs cache.
func NewResolver(cfg config.Config, logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Resolver{
		cache:       NewCache(cfg.ImagesDir()),
		mode:        cfg.ImagesMode,
		snapshotter: cfg.ContainerdSnapshotter,
		dataRoot:    containerdDataRoot(),
		logger:      logger,
		refs:        map[string]string{},
		cacheOnly:   map[string]time.Time{},
	}
	r.policy, r.policyErr = NewPolicy(cfg, logger)
	if r.mode == "" {
		r.mode = config.ImagesAuto
	}
	if r.snapshotter == "" {
		r.snapshotter = vctr.DefaultSnapshotter
	}
	switch {
	case r.mode == config.ImagesCache:
	case vctr.Available(cfg.ContainerdAddress):
		cli, err := vctr.Connect(cfg.ContainerdAddress, cfg.ContainerdNamespace)
		if err != nil {
			r.logger.Warn("containerd unavailable; using the local image cache", "error", err)
		} else {
			r.cli = cli
			r.fromContainerd = r.containerdRootfs
		}
	default:
		if r.mode == config.ImagesContainerd {
			r.logger.Warn("containerd requested but no socket is available")
		}
	}
	return r
}

// Cache exposes the underlying local cache (tests and scripts).
func (r *Resolver) Cache() *Cache { return r.cache }

// UsesContainerd reports whether containerd is the active resolver.
func (r *Resolver) UsesContainerd() bool { return r.cli != nil }

// Close releases the containerd connection.
func (r *Resolver) Close() error { return vctr.Close(r.cli) }

// Rootfs returns the local-cache rootfs path for a ref. It is the placeholder
// recorded at create time; Resolve returns the effective path at start time.
func (r *Resolver) Rootfs(ref string) string { return r.cache.Rootfs(ref) }

// Resolve returns a read-only rootfs directory for ref, pulling and unpacking
// the image through containerd if necessary.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	if Name(ref) == "" {
		return "", fmt.Errorf("images: invalid image reference %q", ref)
	}
	// A misconfigured policy (enforce without a key/cosign) refuses every
	// image rather than silently trusting them.
	if r.policyErr != nil {
		return "", r.policyErr
	}
	if r.policy.Enabled() {
		named, err := reference.ParseNormalizedNamed(ref)
		if err != nil {
			return "", fmt.Errorf("images: invalid image reference: %w", err)
		}
		if _, ok := named.(reference.Digested); !ok {
			return "", fmt.Errorf("images: signature enforcement requires a digest-pinned image reference")
		}
		if r.cli == nil {
			return "", fmt.Errorf("images: signature enforcement requires containerd; the local cache is not verified")
		}
	}
	// Verify the immutable digest before pulling or unpacking its content.
	if err := r.policy.Verify(ctx, ref); err != nil {
		return "", err
	}
	r.mu.Lock()
	cached, ok := r.refs[ref]
	r.mu.Unlock()
	if ok {
		return cached, nil
	}

	tried := r.fromContainerd != nil && r.containerdDue(ref)
	if tried {
		path, err := r.fromContainerd(ctx, ref)
		if err == nil {
			r.mu.Lock()
			r.refs[ref] = path
			delete(r.cacheOnly, ref)
			r.mu.Unlock()
			return path, nil
		}
		if r.mode == config.ImagesContainerd || r.policy.Enabled() {
			return "", err
		}
		r.logger.Debug("containerd resolve failed; falling back to the image cache", "image", ref, "error", err)
	}
	path, err := r.cache.Resolve(ref)
	if err == nil && tried {
		r.mu.Lock()
		r.cacheOnly[ref] = time.Now().Add(containerdRetryAfter)
		r.mu.Unlock()
	}
	return path, err
}

// containerdDue reports whether ref may try containerd. A mode or policy that accepts
// nothing but containerd returns before the cache, so it never holds a ref back.
func (r *Resolver) containerdDue(ref string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().After(r.cacheOnly[ref])
}

func (r *Resolver) containerdRootfs(ctx context.Context, ref string) (string, error) {
	img, err := r.getImage(ctx, ref)
	if err != nil {
		name := normalizedRef(ref)
		img, err = r.cli.Pull(ctx, name,
			ctrclient.WithPullUnpack,
			ctrclient.WithPullSnapshotter(r.snapshotter),
		)
		if err != nil {
			return "", fmt.Errorf("images: pull %s: %w", name, err)
		}
	}
	unpacked, err := img.IsUnpacked(ctx, r.snapshotter)
	if err != nil {
		return "", fmt.Errorf("images: inspect %s: %w", ref, err)
	}
	if !unpacked {
		if err := img.Unpack(ctx, r.snapshotter); err != nil {
			return "", fmt.Errorf("images: unpack %s: %w", ref, err)
		}
	}
	diffs, err := img.RootFS(ctx)
	if err != nil {
		return "", fmt.Errorf("images: rootfs %s: %w", ref, err)
	}
	key := identity.ChainID(diffs).String()
	mounts, err := r.imageMounts(ctx, key)
	if err != nil {
		return "", fmt.Errorf("images: snapshot %s: %w", ref, err)
	}
	for i := range mounts {
		mounts[i].Source = r.hostPath(mounts[i].Source)
	}
	return rootfsDir(mounts, filepath.Join(r.cache.Root(), ".mounts", sanitizeKey(ref)))
}

// hostPath rewrites a containerd runtime path to its host-visible equivalent.
// A rootless containerd reports paths under /var/lib/containerd, but that path
// is only valid inside containerd's own mount namespace. bwrap sandboxes run
// under the user systemd manager in the host namespace, so the rootfs must be
// referenced by its real path under $XDG_DATA_HOME.
func (r *Resolver) hostPath(p string) string {
	const runtimeRoot = "/var/lib/containerd"
	if p != runtimeRoot && !strings.HasPrefix(p, runtimeRoot+"/") {
		return p
	}
	if r.dataRoot == "" {
		return p
	}
	alt := filepath.Join(r.dataRoot, strings.TrimPrefix(strings.TrimPrefix(p, runtimeRoot), "/"))
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	return p
}

// containerdDataRoot is the host-side rootless containerd data directory.
func containerdDataRoot() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "containerd")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "containerd")
	}
	return ""
}

// imageMounts returns the mounts for a read-only view of an unpacked image.
// The image's own snapshot is committed, and containerd only mounts active or
// view snapshots, so we create a persistent view child keyed by the chain id.
// For the native snapshotter the view resolves to the parent's plain rootfs
// directory, which both backends can bind without an extra mount.
func (r *Resolver) imageMounts(ctx context.Context, chainID string) ([]mount.Mount, error) {
	sn := r.cli.SnapshotService(r.snapshotter)
	viewKey := "vora-rootfs-" + sanitizeKey(chainID)
	if _, err := sn.Stat(ctx, viewKey); err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, err
		}
		if _, err := sn.View(ctx, viewKey, chainID); err != nil && !errdefs.IsAlreadyExists(err) {
			return nil, err
		}
	}
	return sn.Mounts(ctx, viewKey)
}

// getImage looks the ref up under every plausible containerd name.
func (r *Resolver) getImage(ctx context.Context, ref string) (ctrclient.Image, error) {
	var lastErr error
	for _, name := range candidateRefs(ref) {
		img, err := r.cli.GetImage(ctx, name)
		if err == nil {
			return img, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// rootfsDir returns the snapshot rootfs directory. The native snapshotter
// exposes it as a plain directory; other snapshotters are mounted into a
// Vora-owned target.
func rootfsDir(mounts []mount.Mount, fallback string) (string, error) {
	for _, m := range mounts {
		if m.Source == "" {
			continue
		}
		for _, src := range []string{m.Source, rootlessHostPath(m.Source)} {
			if info, err := os.Stat(src); src != "" && err == nil && info.IsDir() {
				return src, nil
			}
		}
	}
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		return "", err
	}
	if err := mount.All(mounts, fallback); err != nil {
		return "", fmt.Errorf("images: mount snapshot rootfs: %w", err)
	}
	return fallback, nil
}

// containerdNamespaceRoot is where rootless containerd sees its own data
// directory: containerd-rootless.sh bind-mounts $XDG_DATA_HOME/containerd there
// inside its private mount namespace, and reports snapshot paths under it.
const containerdNamespaceRoot = "/var/lib/containerd/"

// rootlessHostPath maps a snapshot path reported by rootless containerd to the
// same directory as seen from the host, or "" if src is not under its data
// root. The runner reaches that containerd through the socket of the user it
// runs as, so the data root is the runner's own $XDG_DATA_HOME/containerd.
// Sandboxes start in two mount namespaces (the runner's and the user
// manager's), so the host path is the one both can use.
func rootlessHostPath(src string) string {
	rel, ok := strings.CutPrefix(src, containerdNamespaceRoot)
	if !ok {
		return ""
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "containerd", filepath.Clean("/"+rel))
}

// candidateRefs lists the names a ref may be stored under in containerd.
func candidateRefs(ref string) []string {
	raw := strings.TrimSpace(ref)
	names := []string{raw, normalizedRef(raw)}
	// A short name may also be stored verbatim with a tag.
	if !strings.Contains(lastPathComponent(raw), ":") && !strings.Contains(raw, "@") {
		names = append(names, raw+":latest")
	}
	return dedupe(names)
}

// normalizedRef returns the canonical containerd name for ref.
func normalizedRef(ref string) string {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(ref))
	if err != nil {
		return strings.TrimSpace(ref)
	}
	named = reference.TagNameOnly(named)
	return named.String()
}

func lastPathComponent(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sanitizeKey(ref string) string {
	return strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(ref)
}

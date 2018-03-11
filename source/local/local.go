package local

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/boltdb/bolt"
	"github.com/genuinetools/img/fsutils"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/contenthash"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/source"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/tonistiigi/fsutil"
)

const keySharedKey = "local.sharedKey"

// Opt contains the options for the local source.
type Opt struct {
	CacheAccessor cache.Accessor
	MetadataStore *metadata.Store
	LocalDirs     map[string]string
}

// NewSource returns a new source object.
func NewSource(opt Opt) (source.Source, error) {
	ls := &localSource{
		cm: opt.CacheAccessor,
		md: opt.MetadataStore,
		ld: opt.LocalDirs,
	}
	return ls, nil
}

type localSource struct {
	cm cache.Accessor
	md *metadata.Store
	ld map[string]string
}

func (ls *localSource) ID() string {
	return source.LocalScheme
}

func (ls *localSource) Resolve(ctx context.Context, id source.Identifier) (source.SourceInstance, error) {
	localIdentifier, ok := id.(*source.LocalIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid local identifier %v", id)
	}

	return &localSourceHandler{
		src:         *localIdentifier,
		localSource: ls,
	}, nil
}

type localSourceHandler struct {
	src source.LocalIdentifier
	*localSource
}

func (ls *localSourceHandler) CacheKey(ctx context.Context) (string, error) {
	sessionID := ls.src.SessionID

	if sessionID == "" {
		id := session.FromContext(ctx)
		if id == "" {
			return "", errors.New("could not access local files without session")
		}
		sessionID = id
	}
	dt, err := json.Marshal(struct {
		SessionID       string
		IncludePatterns []string
		ExcludePatterns []string
	}{SessionID: sessionID, IncludePatterns: ls.src.IncludePatterns, ExcludePatterns: ls.src.ExcludePatterns})
	if err != nil {
		return "", err
	}
	return "session:" + ls.src.Name + ":" + digest.FromBytes(dt).String(), nil
}

func (ls *localSourceHandler) Snapshot(ctx context.Context) (out cache.ImmutableRef, retErr error) {
	id := session.FromContext(ctx)
	if id == "" {
		return nil, errors.New("could not access local files without session")
	}

	sharedKey := keySharedKey + ":" + ls.src.Name + ":" + ls.src.SharedKeyHint + ":" + ls.ld[ls.src.Name]

	var mutable cache.MutableRef
	sis, err := ls.md.Search(sharedKey)
	if err != nil {
		return nil, err
	}
	for _, si := range sis {
		if m, err := ls.cm.GetMutable(ctx, si.ID()); err == nil {
			logrus.Debugf("reusing ref for local: %s", m.ID())
			mutable = m
			break
		}
	}

	if mutable == nil {
		m, err := ls.cm.New(ctx, nil, cache.CachePolicyRetain, cache.WithDescription(fmt.Sprintf("local source for %s", ls.src.Name)))
		if err != nil {
			return nil, err
		}
		mutable = m
		logrus.Debugf("new ref for local: %s", mutable.ID())
	}

	defer func() {
		if retErr != nil && mutable != nil {
			go mutable.Release(context.TODO())
		}
	}()

	mount, err := mutable.Mount(ctx, false)
	if err != nil {
		return nil, err
	}

	lm := snapshot.LocalMounter(mount)

	dest, err := lm.Mount()
	if err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()

	cc, err := contenthash.GetCacheContext(ctx, mutable.Metadata())
	if err != nil {
		return nil, err
	}

	if err := fsutils.CopyDir(ls.ld[ls.src.Name], dest, ls.src, &cacheUpdater{cc}); err != nil {
		return nil, err
	}

	if err := lm.Unmount(); err != nil {
		return nil, err
	}
	lm = nil

	if err := contenthash.SetCacheContext(ctx, mutable.Metadata(), cc); err != nil {
		return nil, err
	}

	// skip storing snapshot by the shared key if it already exists
	skipStoreSharedKey := false
	si, _ := ls.md.Get(mutable.ID())
	if v := si.Get(keySharedKey); v != nil {
		var str string
		if err := v.Unmarshal(&str); err != nil {
			return nil, err
		}
		skipStoreSharedKey = str == sharedKey
	}
	if !skipStoreSharedKey {
		v, err := metadata.NewValue(sharedKey)
		if err != nil {
			return nil, err
		}
		v.Index = sharedKey
		if err := si.Update(func(b *bolt.Bucket) error {
			return si.SetValue(b, sharedKey, v)
		}); err != nil {
			return nil, err
		}
		logrus.Debugf("saved %s as %s", mutable.ID(), sharedKey)
	}

	snap, err := mutable.Commit(ctx)
	if err != nil {
		return nil, err
	}

	mutable = nil // avoid deferred cleanup

	return snap, nil
}

type cacheUpdater struct {
	contenthash.CacheContext
}

func (cu *cacheUpdater) MarkSupported(bool) {
}

func (cu *cacheUpdater) ContentHasher() fsutil.ContentHasher {
	return contenthash.NewFromStat
}

package server

// tier is a set of artifacts with one rule for who writes and who reads them.
type tier string

const (
	// tierStatic holds artifacts written with static access tokens, at the
	// bucket root; it is the only tier when token exchange is off.
	tierStatic tier = "static"
	// tierTrusted holds the artifacts a repository's trusted branches wrote.
	tierTrusted tier = "trusted"
	// tierScoped holds the artifacts written under one namespace of a repository.
	tierScoped tier = "scoped"
)

// repository identifies one repository of one CI provider.
type repository struct {
	provider string
	id       string
}

func (r repository) String() string {
	return r.provider + "/" + r.id
}

// credential is what a request's bearer token grants.
type credential struct {
	write bool
	// tier is the tier the credential writes to; a read-only credential reads it.
	tier tier
	// repo and namespace are set for repository tiers; namespace only for the
	// scoped tier.
	repo      repository
	namespace string
	// runID and actor identify the CI run an issued token was issued to.
	runID string
	actor string
}

// location is one storage key a download may be served from.
type location struct {
	tier tier
	key  string
}

// reads lists, in lookup order, where a download of hash may be found. A
// scoped credential reads its repository's trusted tier first: most PR tasks
// match the trusted branch, and the trusted copy wins when both exist.
func (c *credential) reads(hash string) []location {
	switch c.tier {
	case tierTrusted:
		return []location{{tierTrusted, trustedKey(c.repo, hash)}}
	case tierScoped:
		return []location{
			{tierTrusted, trustedKey(c.repo, hash)},
			{tierScoped, scopedKey(c.repo, c.namespace, hash)},
		}
	case tierStatic:
	}
	return []location{{tierStatic, hash}}
}

// writeKey is the storage key an upload of hash goes to. Hashes cannot
// contain "/", so keys of different tiers never collide.
func (c *credential) writeKey(hash string) string {
	switch c.tier {
	case tierTrusted:
		return trustedKey(c.repo, hash)
	case tierScoped:
		return scopedKey(c.repo, c.namespace, hash)
	case tierStatic:
	}
	return hash
}

func trustedKey(repo repository, hash string) string {
	return "trusted/" + repo.provider + "/" + repo.id + "/" + hash
}

func scopedKey(repo repository, namespace, hash string) string {
	return "scoped/" + repo.provider + "/" + repo.id + "/" + namespace + "/" + hash
}

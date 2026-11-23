package mcpserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// nearSlugs bounds the listing a not_found looks through for a near match.
const nearSlugs = 200

// notFound names the slug a call asked for in its not_found error, and the
// closest slug the caller can see when one is close: most misses are typos
// or a slug remembered wrong.
func (b *builder) notFound(ctx context.Context, actor artifact.Actor, req *mcp.CallToolRequest, err error) error {
	if !errors.Is(err, artifact.ErrNotFound) || req == nil || req.Params == nil {
		return err
	}
	var args struct {
		Slug string `json:"slug"`
	}
	if json.Unmarshal(req.Params.Arguments, &args) != nil || args.Slug == "" {
		return err
	}
	msg := "artifact " + quote(args.Slug) + " not found"
	page, lerr := b.d.Artifacts.List(ctx, actor, artifact.ListQuery{Mine: "", Limit: nearSlugs})
	if lerr == nil {
		if near := nearest(args.Slug, page.Items); near != "" {
			msg += "; did you mean " + quote(near) + "?"
		}
	}
	return artifact.Errorf(artifact.ErrNotFound, "%s", msg)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// nearest is the slug of items closest to slug by edit distance, if within
// a third of its length.
func nearest(slug string, items []artifact.Artifact) string {
	best, bestDist := "", len(slug)/3+1
	for _, a := range items {
		if a.Slug == slug {
			continue
		}
		if d := distance(slug, a.Slug, bestDist); d < bestDist {
			best, bestDist = a.Slug, d
		}
	}
	return best
}

// distance is the Levenshtein distance of a and b, or limit when it is at
// least limit.
func distance(a, b string, limit int) int {
	if abs(len(a)-len(b)) >= limit {
		return limit
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		low := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			low = min(low, cur[j])
		}
		if low >= limit {
			return limit
		}
		prev, cur = cur, prev
	}
	return min(prev[len(b)], limit)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

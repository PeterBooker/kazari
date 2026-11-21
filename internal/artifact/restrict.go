package artifact

import (
	"regexp"
	"slices"
	"strings"
)

// MaxSlugPatterns is how many patterns one credential's restriction holds.
const MaxSlugPatterns = 20

// slugWildcard ends a pattern that matches every slug starting with the rest.
const slugWildcard = "*"

var slugPrefixRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateSlugPattern accepts an exact slug, or a prefix of one followed by
// "*" ("reports-*"). A lone "*" is refused: a credential for every artifact
// is one without a restriction.
func ValidateSlugPattern(p string) error {
	prefix, wild := strings.CutSuffix(p, slugWildcard)
	if !wild {
		return ValidateSlug(p, nil)
	}
	switch {
	case prefix == "":
		return Errorf(ErrInvalidSlug, "pattern %q matches every slug; leave the restriction out instead", p)
	case len(prefix) > SlugMaxLen:
		return Errorf(ErrInvalidSlug, "pattern %q is longer than a slug can be", p)
	case !slugPrefixRe.MatchString(prefix) || strings.Contains(prefix, "--"):
		return Errorf(ErrInvalidSlug, "pattern %q must be a slug, or the start of one followed by \"*\"", p)
	}
	return nil
}

// ValidateSlugPatterns checks every pattern and their number.
func ValidateSlugPatterns(patterns []string) error {
	if len(patterns) > MaxSlugPatterns {
		return Errorf(ErrInvalidArgument, "%d slug patterns exceeds the limit of %d", len(patterns), MaxSlugPatterns)
	}
	for _, p := range patterns {
		if err := ValidateSlugPattern(p); err != nil {
			return err
		}
	}
	return nil
}

// MatchSlug reports whether slug matches one of patterns. No patterns means
// no restriction, so everything matches.
func MatchSlug(patterns []string, slug string) bool {
	if len(patterns) == 0 {
		return true
	}
	return slices.ContainsFunc(patterns, func(p string) bool { return matchSlugPattern(p, slug) })
}

func matchSlugPattern(pattern, slug string) bool {
	if prefix, wild := strings.CutSuffix(pattern, slugWildcard); wild {
		return strings.HasPrefix(slug, prefix)
	}
	return pattern == slug
}

// SlugPatternsWithin reports whether every slug inner matches also matches
// outer, so a credential restricted to inner is no wider than one restricted
// to outer. An empty outer is no restriction and holds everything; an empty
// inner fits only in that.
func SlugPatternsWithin(inner, outer []string) bool {
	if len(outer) == 0 {
		return true
	}
	if len(inner) == 0 {
		return false
	}
	for _, in := range inner {
		if !slices.ContainsFunc(outer, func(out string) bool { return patternWithin(in, out) }) {
			return false
		}
	}
	return true
}

func patternWithin(in, out string) bool {
	outPrefix, outWild := strings.CutSuffix(out, slugWildcard)
	if !outWild {
		return in == out
	}
	return strings.HasPrefix(strings.TrimSuffix(in, slugWildcard), outPrefix)
}

// checkSlug refuses a slug outside the actor's restriction. Every method
// that takes an actor and a slug calls it first, before the artifact is
// looked up, so a restricted credential learns nothing about other slugs.
func checkSlug(actor Actor, slug string) error {
	if MatchSlug(actor.Slugs, slug) {
		return nil
	}
	return Errorf(ErrForbidden, "this credential is restricted to slugs matching %s; %q is outside them",
		strings.Join(actor.Slugs, ", "), slug)
}

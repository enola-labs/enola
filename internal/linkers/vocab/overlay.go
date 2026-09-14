package vocab

import "fmt"

// Overlay is the user-supplied `linking:` block from enola.yaml, applied on top of
// Default().
//
// MERGE SEMANTICS. Every list field is ADD/REMOVE rather than replace, and that is the
// whole design. A replacing list is a trap: a user adding one framework's base class
// silently discards every default, so the fix for one false edge quietly reintroduces
// all the others — and nothing fails, because a linker with a thinner vocabulary just
// draws more edges. Extractors already carry this scar (see Config.ExtractorsExplicit,
// which exists to detect exactly that mistake after the fact). Here the shape prevents
// it instead.
//
// Thresholds are pointers so "not mentioned" is distinguishable from "set to zero" —
// with a plain int, omitting min_shared_symbols would mean 0 and every incidental name
// collision would become an edge.
type Overlay struct {
	GenericPathSegments   ListOverlay `yaml:"generic_path_segments"`
	GenericTypeNames      ListOverlay `yaml:"generic_type_names"`
	FrameworkConventions  ListOverlay `yaml:"framework_conventions"`
	GenericComponentNames ListOverlay `yaml:"generic_component_names"`
	ConventionSuffixes    ListOverlay `yaml:"convention_suffixes"`
	NonContractPaths      ListOverlay `yaml:"non_contract_paths"`
	FormatExtensions      ListOverlay `yaml:"format_extensions"`

	TopicOwnerSeparator string `yaml:"topic_owner_separator"`

	// MatchLiteralAgainstParams turns literal-against-parameter route matching on. Nil
	// leaves it off; see Set.MatchLiteralAgainstParams.
	MatchLiteralAgainstParams *bool `yaml:"match_literal_against_params"`

	Thresholds ThresholdOverlay `yaml:"thresholds"`
}

// ListOverlay adds to and removes from one built-in list.
type ListOverlay struct {
	Add    []string `yaml:"add"`
	Remove []string `yaml:"remove"`
}

// Empty reports whether this overlay would change anything.
func (l ListOverlay) Empty() bool { return len(l.Add) == 0 && len(l.Remove) == 0 }

// ThresholdOverlay overrides individual numeric gates. A nil field means "leave the
// default alone" — see the Overlay doc for why these are pointers.
type ThresholdOverlay struct {
	MinSharedSegments       *int     `yaml:"min_shared_segments"`
	MinSharedSymbols        *int     `yaml:"min_shared_symbols"`
	MaxVocabRepoShare       *float64 `yaml:"max_vocab_repo_share"`
	MinReposForVocabFilter  *int     `yaml:"min_repos_for_vocab_filter"`
	MinFileSimilarity       *float64 `yaml:"min_file_similarity"`
	MaxComparedFileBytes    *int     `yaml:"max_compared_file_bytes"`
	MaxFactsPerIdentityRepo *int     `yaml:"max_facts_per_identity_repo"`
	MaxSamples              *int     `yaml:"max_samples"`
}

// Apply returns Default() with the overlay applied, or an error describing every
// problem it found.
//
// It VALIDATES rather than clamping. A threshold set to zero or a negative is almost
// certainly a typo, and silently correcting it would leave the user believing a setting
// took effect that did not — while silently accepting it can only widen what links,
// which is the direction that fabricates edges. Erroring at load is the only outcome
// that tells the truth.
func Apply(o *Overlay) (*Set, error) {
	s := Default()
	if o == nil {
		return s, nil
	}

	applySet(s.GenericPathSegments, o.GenericPathSegments)
	applySet(s.GenericTypeNames, o.GenericTypeNames)
	applySet(s.FrameworkConventions, o.FrameworkConventions)
	applySet(s.GenericComponentNames, o.GenericComponentNames)
	s.ConventionSuffixes = applyList(s.ConventionSuffixes, o.ConventionSuffixes)
	s.NonContractPaths = applyList(s.NonContractPaths, o.NonContractPaths)
	s.FormatExtensions = applyList(s.FormatExtensions, o.FormatExtensions)

	if o.TopicOwnerSeparator != "" {
		s.TopicOwnerSeparator = o.TopicOwnerSeparator
	}
	if o.MatchLiteralAgainstParams != nil {
		s.MatchLiteralAgainstParams = *o.MatchLiteralAgainstParams
	}

	var errs []error
	t := &s.Thresholds
	setInt := func(dst *int, src *int, name string, min int) {
		if src == nil {
			return
		}
		if *src < min {
			errs = append(errs, fmt.Errorf("linking.thresholds.%s = %d, must be >= %d", name, *src, min))
			return
		}
		*dst = *src
	}
	setFrac := func(dst *float64, src *float64, name string) {
		if src == nil {
			return
		}
		if *src <= 0 || *src > 1 {
			errs = append(errs, fmt.Errorf("linking.thresholds.%s = %v, must be within (0, 1]", name, *src))
			return
		}
		*dst = *src
	}
	setInt(&t.MinSharedSegments, o.Thresholds.MinSharedSegments, "min_shared_segments", 1)
	setInt(&t.MinSharedSymbols, o.Thresholds.MinSharedSymbols, "min_shared_symbols", 1)
	setInt(&t.MinReposForVocabFilter, o.Thresholds.MinReposForVocabFilter, "min_repos_for_vocab_filter", 2)
	setInt(&t.MaxComparedFileBytes, o.Thresholds.MaxComparedFileBytes, "max_compared_file_bytes", 1)
	setInt(&t.MaxFactsPerIdentityRepo, o.Thresholds.MaxFactsPerIdentityRepo, "max_facts_per_identity_repo", 1)
	setInt(&t.MaxSamples, o.Thresholds.MaxSamples, "max_samples", 1)
	setFrac(&t.MaxVocabRepoShare, o.Thresholds.MaxVocabRepoShare, "max_vocab_repo_share")
	setFrac(&t.MinFileSimilarity, o.Thresholds.MinFileSimilarity, "min_file_similarity")

	if len(errs) > 0 {
		return nil, joinErrors(errs)
	}
	return s, nil
}

func applySet(dst map[string]bool, o ListOverlay) {
	for _, v := range o.Add {
		dst[v] = true
	}
	for _, v := range o.Remove {
		delete(dst, v)
	}
}

func applyList(dst []string, o ListOverlay) []string {
	drop := make(map[string]bool, len(o.Remove))
	for _, v := range o.Remove {
		drop[v] = true
	}
	out := make([]string, 0, len(dst)+len(o.Add))
	for _, v := range dst {
		if !drop[v] {
			out = append(out, v)
		}
	}
	for _, v := range o.Add {
		if !drop[v] {
			out = append(out, v)
		}
	}
	return out
}

func joinErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	err := errs[0]
	for _, e := range errs[1:] {
		err = fmt.Errorf("%w; %w", err, e)
	}
	return err
}

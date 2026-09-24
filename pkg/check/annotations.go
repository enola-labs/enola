package check

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Annotations renders every positioned finding as an entry the host shows on
// the diff: Buildkite markdown grouped by file, or GitHub workflow commands.
// Findings without a position are counted at the end, never placed, so a
// module-level finding stays in the summary instead of being pinned to a
// line nobody wrote. link is an optional base URL of the pull request's files
// view; with it each Buildkite entry links the line in the diff.
func (v Verdict) Annotations(host Host, link string) ([]byte, error) {
	switch host {
	case HostBuildkite:
		return []byte(v.buildkiteAnnotations(link)), nil
	case HostGitHub:
		return []byte(v.githubAnnotations()), nil
	}
	return nil, fmt.Errorf("unknown host %q", host)
}

// entryLine is the one sentence both hosts show for a finding: the bucket,
// the rule, the title, the reason, and the cut when the explainer computed one.
func (v Verdict) entryLine(p placed) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s: %s", p.bucket.name, ruleOf(p.finding), oneLine(p.finding.Title))
	if r := reasonOf(p.finding); r != "" {
		fmt.Fprintf(&b, " Because: %s", r)
	}
	if a := actionOf(p.finding); a != "" {
		fmt.Fprintf(&b, " Action: %s", a)
	}
	if e := v.excuseOf(p.bucket.name, p.finding); e != "" {
		fmt.Fprintf(&b, " (%s)", e)
	}
	return b.String()
}

func (v Verdict) buildkiteAnnotations(link string) string {
	var sb strings.Builder
	file := ""
	placedCount, unplaced := 0, 0
	for _, p := range v.placements() {
		if !p.located || p.bucket.name == "resolved" {
			unplaced++
			continue
		}
		placedCount++
		path := v.sources.hostPath(p.evidence.File)
		if path != file {
			if file != "" {
				sb.WriteString("\n")
			}
			fmt.Fprintf(&sb, "### %s\n\n", path)
			file = path
		}
		where := fmt.Sprintf("%s:%d", path, p.evidence.Line)
		if link != "" {
			where = fmt.Sprintf("[%s](%s)", where, diffAnchor(link, path, p.evidence.Line))
		}
		fmt.Fprintf(&sb, "- %s %s\n", where, v.entryLine(p))
	}
	if placedCount == 0 && unplaced == 0 {
		sb.WriteString("No findings.\n")
	}
	if unplaced > 0 {
		if placedCount > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "%s without a position stay in the summary.\n", plural(unplaced, "finding", "findings"))
	}
	return sb.String()
}

// diffAnchor is the anchor GitHub gives a file in a pull request's files view:
// the hex SHA-256 of its path, with the line on the right-hand side.
func diffAnchor(base, path string, line int) string {
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%s#diff-%sR%d", strings.TrimRight(base, "/"), hex.EncodeToString(sum[:]), line)
}

// Workflow commands are one line each, so the grammar escapes what would end
// the line or the property list. The data and the property values escape
// different sets; both tables are the ones GitHub's toolkit documents.
var (
	commandData       = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	commandProperties = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
)

func (v Verdict) githubAnnotations() string {
	var sb strings.Builder
	unplaced := 0
	for _, p := range v.placements() {
		if !p.located || p.bucket.name == "resolved" {
			unplaced++
			continue
		}
		command := map[string]string{levelError: "error", levelWarning: "warning", levelNote: "notice"}[p.bucket.level]
		props := []string{
			"file=" + commandProperties.Replace(v.sources.hostPath(p.evidence.File)),
			fmt.Sprintf("line=%d", p.evidence.Line),
		}
		if p.evidence.EndLine >= p.evidence.Line && p.evidence.EndLine > 0 {
			props = append(props, fmt.Sprintf("endLine=%d", p.evidence.EndLine))
		}
		if p.evidence.Column > 0 {
			props = append(props, fmt.Sprintf("col=%d", p.evidence.Column))
		}
		if p.evidence.EndColumn > 0 {
			props = append(props, fmt.Sprintf("endColumn=%d", p.evidence.EndColumn))
		}
		props = append(props, "title="+commandProperties.Replace(ruleOf(p.finding)))
		fmt.Fprintf(&sb, "::%s %s::%s\n", command, strings.Join(props, ","), commandData.Replace(v.entryLine(p)))
	}
	if unplaced > 0 {
		fmt.Fprintf(&sb, "::notice ::%s without a position stay in the summary.\n", plural(unplaced, "finding", "findings"))
	}
	return sb.String()
}

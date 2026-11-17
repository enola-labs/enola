package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// After an expression-bodied function the grammar reads the next annotated
// expression-bodied declarations as statements. They come back as declarations,
// at their own lines, with their annotations' roles.
func TestMisreadDeclarationsAfterAnExpressionBody(t *testing.T) {
	src := `package p

private fun Dialog.style(): Style = when {
    this is FullScreen &&
            type == Kind.COVER -> Style.COVER

    this is FullScreen -> Style.FULLSCREEN
    else -> Style.MODAL
}

@PreviewSnapshot
@PreviewLightDark
@Composable
private fun ModalDialogPreview() = PreviewDialog(sampleModal())

@PreviewSnapshot(renderingMode = PreviewSnapshot.RenderingMode.NORMAL)
@PreviewLightDark
@Composable
private fun SplashDialogPreview() =
        PreviewDialog(sampleSplash(Kind.SPLASH))

private fun sampleModal() = Dialog()
`
	got := map[string]facts.Fact{}
	for _, f := range extractAST(t, src, true) {
		if f.Kind == facts.KindSymbol {
			got[f.Name] = f
		}
	}
	for name, line := range map[string]int{"pkg.ModalDialogPreview": 11, "pkg.SplashDialogPreview": 16, "pkg.style": 3, "pkg.sampleModal": 22} {
		f, ok := got[name]
		if !ok {
			t.Errorf("missing %s among %v", name, keys(got))
			continue
		}
		if f.Line != line {
			t.Errorf("%s at line %d, want %d", name, f.Line, line)
		}
	}
	if f := got["pkg.ModalDialogPreview"]; f.PropString("android_component") != "composable" {
		t.Errorf("recovered preview lost its @Composable role: %v", f.Props)
	}
	calls := false
	for _, r := range got["pkg.ModalDialogPreview"].Relations {
		calls = calls || (r.Kind == facts.RelCalls && r.Target == "pkg.sampleModal")
	}
	if !calls {
		t.Errorf("recovered preview lost its call to sampleModal: %v", got["pkg.ModalDialogPreview"].Relations)
	}
}

func keys(m map[string]facts.Fact) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

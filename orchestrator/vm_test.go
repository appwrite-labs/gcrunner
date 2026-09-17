package function

import "testing"

// A workflow pins one build of the runner image by naming it in the image label.
func TestImageLabelNamesAnImageInTheImageProject(t *testing.T) {
	t.Setenv("GCRUNNER_IMAGE_PROJECT", "appwrite-gha-runners")

	cases := map[string]string{
		"ubuntu24-full-x64": "projects/appwrite-gha-runners/global/images/family/gcrunner-ubuntu2404-x64",
		"gcrunner-ubuntu2404-x64-1fe0552-20260917140000": "projects/appwrite-gha-runners/global/images/gcrunner-ubuntu2404-x64-1fe0552-20260917140000",
		"projects/other/global/images/family/custom":     "projects/other/global/images/family/custom",
	}
	for label, want := range cases {
		if got := resolveSourceImage(label); got != want {
			t.Errorf("resolveSourceImage(%q) = %q, want %q", label, got, want)
		}
	}
}

package docker_test

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfileUsesCanonicalBuilderIdentity(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(contents)
	for _, required := range []string{
		"ARG VEQRI_VERSION=0.1.0-dev",
		"ARG VEQRI_COMMIT=unknown",
		"ARG VEQRI_BUILD_TIME=unknown",
		"ARG VEQRI_RELEASE=false",
		"go run ./cmd/veqri-build $release_flag --strip binaries",
		"COPY --from=build /src/build/release/buildinfo.json /usr/share/veqri/buildinfo.json",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"org.opencontainers.image.version",
		"org.opencontainers.image.revision",
		"org.opencontainers.image.created",
	} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("Dockerfile must not publish unnormalized metadata through %q", forbidden)
		}
	}
}

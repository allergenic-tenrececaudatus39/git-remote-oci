package test

import (
	"os"
	"strings"
)

// The suite runs against a registry it starts itself, and which registry that
// is used to be `registry:2`, hardcoded in three places.
//
// E2E_REGISTRY_IMAGE swaps it. The default is Distribution v3 (`registry:3`),
// the current CNCF reference implementation; `registry:2` is the previous major
// and reads the same REGISTRY_* environment, so it works as a drop-in if you
// want it. Anything further afield — zot, Harbor — configures differently,
// which is what E2E_REGISTRY_ARGS is for.
const (
	registryImageEnv = "E2E_REGISTRY_IMAGE"
	registryArgsEnv  = "E2E_REGISTRY_ARGS"
	registryAuthEnv  = "E2E_REGISTRY_AUTH"

	defaultRegistryImage = "registry:3"

	// How a registry is told to demand credentials, which the distribution
	// spec does not cover and so every implementation does differently.
	authStyleDistribution = "distribution" // REGISTRY_AUTH_* environment
	authStyleZot          = "zot"          // a JSON config file
)

// registryAuthStyle reports how this registry is configured for authentication.
//
// Nothing in the OCI distribution spec says how a registry is *administered*,
// only how it behaves once it is, so this cannot be derived from the image.
// It is declared instead, and the authentication test verifies the declaration
// took effect rather than trusting it: a registry that comes up unauthenticated
// when it was asked not to is caught by the readiness probe.
func registryAuthStyle() string {
	if style := strings.TrimSpace(os.Getenv(registryAuthEnv)); style != "" {
		return style
	}
	return authStyleDistribution
}

// registryIsDefault reports whether the suite is running against the registry
// it is developed against, rather than one it has merely been pointed at.
//
// It decides how strict a test may be about behaviour that is configured
// differently between implementations: the default has to honour what the
// suite asks of it, and anything else is allowed to say "not like that".
func registryIsDefault() bool {
	return registryImage() == defaultRegistryImage
}

// registryImage is the container image the suite starts.
func registryImage() string {
	if img := strings.TrimSpace(os.Getenv(registryImageEnv)); img != "" {
		return img
	}
	return defaultRegistryImage
}

// registryRunArgs builds the `docker run` argument list for a registry that
// this suite drives, with the container named and a random host port.
//
// Deleting a remote ref deletes its OCI manifest, and the distribution images
// disable manifest deletion by default; without that setting the deletion tests
// exercise a registry that cannot do what they are testing. It is passed as an
// environment variable, which is how the distribution family is configured —
// a registry that is not one of those needs E2E_REGISTRY_ARGS instead, and can
// have the variable harmlessly set alongside.
func registryRunArgs(containerName string) []string {
	args := []string{
		"run", "-d", "--name", containerName,
		"-p", "0:5000",
		"-e", "REGISTRY_STORAGE_DELETE_ENABLED=true",
	}
	args = append(args, extraRegistryArgs()...)
	return append(args, registryImage())
}

// extraRegistryArgs is whatever the caller needs to add for a registry that is
// not configured the distribution way. Split on whitespace, which is enough for
// flags and `-e KEY=value` and not enough for anything containing spaces —
// deliberately, because a test harness is the wrong place for a shell parser.
func extraRegistryArgs() []string {
	return strings.Fields(os.Getenv(registryArgsEnv))
}

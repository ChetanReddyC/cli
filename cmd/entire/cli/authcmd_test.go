package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestRenderDataAPIAuthError_DistinguishesCallerContextFromWrappedDeadline
// guards against a regression where any error chain merely satisfying
// errors.Is(err, context.DeadlineExceeded) was treated as the caller's own
// context firing. resolveRepoCellTarget runs under its own internal
// context.WithTimeout, so its timeout error still satisfies that errors.Is
// check even when the caller's context is perfectly live — silencing on that
// basis alone would print nothing for a slow-but-reachable control plane
// (worse than the bug the fail-loud change was meant to fix).
func TestRenderDataAPIAuthError_DistinguishesCallerContextFromWrappedDeadline(t *testing.T) {
	t.Parallel()

	wrappedDeadline := fmt.Errorf("resolve the Entire cell for acme/widget: %w", context.DeadlineExceeded)

	t.Run("live caller context with wrapped DeadlineExceeded is printed, not swallowed", func(t *testing.T) {
		t.Parallel()

		var errW bytes.Buffer
		result := renderDataAPIAuthError(context.Background(), &errW, wrappedDeadline)

		var silent *SilentError
		if errors.As(result, &silent) {
			t.Fatalf("expected a non-silent error for a live caller context, got silent: %v", result)
		}
		if !errors.Is(result, context.DeadlineExceeded) {
			t.Fatalf("expected returned error to still wrap context.DeadlineExceeded, got %v", result)
		}
	})

	t.Run("actually cancelled caller context stays silent", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var errW bytes.Buffer
		result := renderDataAPIAuthError(ctx, &errW, wrappedDeadline)

		var silent *SilentError
		if !errors.As(result, &silent) {
			t.Fatalf("expected a SilentError when the caller's own context is cancelled, got %v", result)
		}
	})
}

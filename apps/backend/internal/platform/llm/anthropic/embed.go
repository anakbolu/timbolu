package anthropic

import (
	"context"
	"fmt"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
)

// Embed reports that this API has no embedding endpoint.
//
// It is honest rather than convenient: the Messages API has no route that
// returns a vector, so a deployment that wants memory retrieval points an
// [OI]-compatible provider at the workspace. Advertising the capability and
// failing at call time would move the failure to the middle of a write.
func (c *Client) Embed(context.Context, domain.EmbedRequest) (domain.EmbedResponse, error) {
	return domain.EmbedResponse{}, fmt.Errorf("%w: %s has no embedding endpoint", domain.ErrUnsupported, c.Name())
}

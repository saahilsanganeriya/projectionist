package jellyfin

import (
	"context"
	"net/http"
)

// Delete sends a DELETE whose answer does not matter, e.g. DELETE /Collections/<id>/Items?ids=… to drop BoxSet members.
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.send(ctx, http.MethodDelete, path, "", nil, nil)
}

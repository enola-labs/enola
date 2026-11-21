package stock

import (
	"context"

	"example.com/petstore/inventoryclient"
)

// Count calls one operation of the generated inventory client; GetItem is never called.
func Count(ctx context.Context, c *inventoryclient.ClientWithResponses) error {
	_, err := c.ListItemsWithResponse(ctx)
	return err
}

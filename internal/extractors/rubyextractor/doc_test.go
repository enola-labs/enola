package rubyextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Ruby symbol's doc is the run of comment lines on the lines above it, up to the
// first YARD tag, without magic comments or linter directives.
func TestDoc_CommentLinesAboveTheDeclaration(t *testing.T) {
	src := []byte(`# frozen_string_literal: true

# Places orders for a user.
# Charges the card on create.
class OrdersController < ApplicationController
  # Creates an order.
  #
  # @param params [Hash] the order
  # @return [Order]
  def create; end

  # rubocop:disable Metrics/AbcSize
  def update; end

  # A note about the section below.

  def destroy; end
end

# Helpers shared by the order views.
module OrdersHelper
end
`)
	got := map[string]string{}
	for _, f := range extractFileAST(src, "app/controllers/orders_controller.rb", true, true) {
		if f.Kind == facts.KindSymbol {
			got[f.Name] = f.PropString(facts.PropDoc)
		}
	}
	want := map[string]string{
		"OrdersController":         "Places orders for a user. Charges the card on create.",
		"OrdersController#create":  "Creates an order.",
		"OrdersController#update":  "",
		"OrdersController#destroy": "",
		"OrdersHelper":             "Helpers shared by the order views.",
	}
	for name, doc := range want {
		if got[name] != doc {
			t.Errorf("%s doc = %q, want %q", name, got[name], doc)
		}
	}
}

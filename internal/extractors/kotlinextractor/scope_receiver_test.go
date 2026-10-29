package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestScopeFunctionReceiver(t *testing.T) {
	ff := extractKotlinRepo(t, map[string]string{
		"app/src/main/java/com/acme/orders/OrdersViewModel.kt": `package com.acme.orders

class OrdersViewModel {
    fun onEnterScreen() {}
    fun onBackClick() {}
}
`,
		"app/src/main/java/com/acme/orders/OrdersFragment.kt": `package com.acme.orders

import androidx.fragment.app.Fragment
import androidx.fragment.app.viewModels

class OrdersFragment : Fragment() {
    private val viewModel: OrdersViewModel by viewModels { factory.create(this) }
    private val plain: OrdersViewModel = OrdersViewModel()

    override fun onStart() {
        viewModel.onEnterScreen()
        plain.onBackClick()
    }

    fun content() {
        with(viewModel) {
            onBackClick()
        }
    }

    fun applied() {
        plain.apply { onEnterScreen() }
    }

    fun wire() {
        render(onBack = viewModel::onBackClick)
    }
}
`,
	})
	for n, want := range map[string]string{
		"OrdersFragment.onStart": "OrdersViewModel.onEnterScreen",
		"OrdersFragment.content": "OrdersViewModel.onBackClick",
		"OrdersFragment.applied": "OrdersViewModel.onEnterScreen",
		"OrdersFragment.wire":    "OrdersViewModel.onBackClick",
	} {
		if f := ktSym(t, ff, n); !ktRelSuffix(f, facts.RelCalls, want) {
			t.Errorf("%s does not call %s: %v", n, want, f.Relations)
		}
	}
}

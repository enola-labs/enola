package phpextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A Route attribute's method is the route's handler. The edge is added only for a
// handler the file's symbol walk declared, which is how php.go combines the two.
func TestSymfonyRoutes_HandledByTheAttributedMethod(t *testing.T) {
	src := `<?php
namespace App\Controller;

use Symfony\Component\Routing\Annotation\Route;

#[Route('/api')]
class UserController
{
    #[Route('/users/{id}', methods: ['GET'])]
    public function show($id) {}
}
`
	rel := "src/Controller/UserController.php"
	fileFacts := extractFileAST([]byte(src), rel)
	routes := bindSymfonyHandlers(extractSymfonyRoutes([]byte(src), rel), fileFacts)
	show := routeByMethodPath(t, routes, "GET", "/api/users/{id}")
	var target string
	for _, r := range show.Relations {
		if r.Kind == facts.RelHandledBy {
			target = r.Target
		}
	}
	if target != `App\Controller\UserController::show` {
		t.Fatalf("handled_by = %q, want the method symbol", target)
	}

	// Without the symbol walk's facts there is nothing to bind to.
	bare := bindSymfonyHandlers(extractSymfonyRoutes([]byte(src), rel), nil)
	for _, r := range routeByMethodPath(t, bare, "GET", "/api/users/{id}").Relations {
		if r.Kind == facts.RelHandledBy {
			t.Fatalf("an undeclared handler was bound: %q", r.Target)
		}
	}
}

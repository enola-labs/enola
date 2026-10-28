package tsextractor

import (
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// reactRouterPages returns path -> handler for the React Router pages of a file.
func reactRouterPages(ff []facts.Fact, file string) map[string]string {
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.File == file && f.Props["router"] == "react-router" {
			h, _ := f.Props["handler"].(string)
			out[f.Name] = h
		}
	}
	return out
}

func samePages(t *testing.T, got, want map[string]string) {
	t.Helper()
	var g, w []string
	for k, v := range got {
		g = append(g, k+" -> "+v)
	}
	for k, v := range want {
		w = append(w, k+" -> "+v)
	}
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		t.Fatalf("pages\n got %v\nwant %v", g, w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("pages\n got %v\nwant %v", g, w)
		}
	}
}

func TestReactRouter_V5ChildrenAndComponent(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/components/SignUp.tsx":   `export default function SignUp() { return null }`,
		"src/components/SignIn.tsx":   `export function SignIn() { return null }`,
		"src/components/Settings.tsx": `export function Settings() { return null }`,
		"src/App.tsx": `import { Switch, Route, Redirect } from "react-router-dom";
import SignUp from "../src/components/SignUp";
import { SignIn } from "./components/SignIn";
import { Settings } from "./components/Settings";
export function App() {
  return (
    <Switch>
      <Route exact path="/signup">
        <SignUp />
      </Route>
      <Route path="/signin" component={SignIn} />
      <Route path="/user">
        <Route path="/user/settings"><Settings /></Route>
      </Route>
      <Route path="/(public|contacts)?"><SignIn /></Route>
      <Route path="/*"><Redirect to="/signin" /></Route>
    </Switch>
  );
}`,
	}, false)
	samePages(t, reactRouterPages(ff, "src/App.tsx"), map[string]string{
		"/signup":        "src/components.SignUp",
		"/signin":        "src/components.SignIn",
		"/user":          "",
		"/user/settings": "src/components.Settings",
	})
}

func TestReactRouter_V6NestingIndexAndLayout(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/pages/Home.tsx":    `export function Home() { return null }`,
		"src/pages/Invoice.tsx": `export function Invoice() { return null }`,
		"src/App.tsx": `import { Routes, Route as R } from "react-router";
import { Home } from "./pages/Home";
import { Invoice } from "./pages/Invoice";
export const App = () => (
  <Routes>
    <R path="/" element={<Layout />}>
      <R index element={<Home />} />
      <R element={<Shell />}>
        <R path="invoices/:id" element={<Invoice />} />
      </R>
    </R>
    <R path="*" element={<NotFound />} />
  </Routes>
);`,
	}, false)
	samePages(t, reactRouterPages(ff, "src/App.tsx"), map[string]string{
		"/":             "src/pages.Home",
		"/invoices/:id": "src/pages.Invoice",
	})
}

func TestReactRouter_RouteObjects(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/pages/Root.tsx":  `export function Root() { return null }`,
		"src/pages/Team.tsx":  `export function Team() { return null }`,
		"src/pages/About.tsx": `export function About() { return null }`,
		"src/router.tsx": `import { createBrowserRouter } from "react-router-dom";
import { Root } from "./pages/Root";
import { Team } from "./pages/Team";
import { About } from "./pages/About";
const routes = [
  { path: "/", element: <Root />, children: [
    { path: "teams/:teamId", Component: Team },
    { path: dynamicPath, element: <Team /> },
  ]},
  { path: "/about", element: <About /> },
];
export const router = createBrowserRouter(routes);`,
	}, false)
	samePages(t, reactRouterPages(ff, "src/router.tsx"), map[string]string{
		"/":              "src/pages.Root",
		"/teams/:teamId": "src/pages.Team",
		"/about":         "src/pages.About",
	})
}

// A Route from another library, a test file's routes, and a file that only mentions
// react-router are not pages.
func TestReactRouter_Negatives(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/Other.tsx":    `import { Route } from "some-router"; export const A = () => <Route path="/x" />`,
		"src/App.test.tsx": `import { Route } from "react-router-dom"; export const B = () => <Route path="/y" />`,
		"src/Note.tsx":     `// uses react-router elsewhere` + "\n" + `export const C = () => <Route path="/z" />`,
		"src/util.ts":      `import { useNavigate } from "react-router-dom"; export const go = () => useNavigate()`,
	}, false)
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props["router"] == "react-router" {
			t.Errorf("page that should not exist: %s @%s", f.Name, f.File)
		}
	}
}

// A *Route wrapper with a literal path is a route; an optional segment names the page
// with and without it.
func TestReactRouter_WrappersAndOptionalSegments(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/pages/About.tsx": `export function About() { return null }`,
		"src/pages/Terms.tsx": `export function Terms() { return null }`,
		"src/App.tsx": `import { Switch, Route } from "react-router-dom";
import { About } from "./pages/About";
import { Terms } from "./pages/Terms";
import { PrivateRoute, ProtectedRoute } from "./auth";
export function App() {
  return (
    <Switch>
      <PrivateRoute path="/about" component={About} />
      <ProtectedRoute><div /></ProtectedRoute>
      <Route path="/terms/:date?" component={Terms} />
      <Route path="/@:acct/tagged/:tag?" />
      <Route path="/(a|b)?" />
    </Switch>
  );
}`,
	}, false)
	samePages(t, reactRouterPages(ff, "src/App.tsx"), map[string]string{
		"/about":              "src/pages.About",
		"/terms":              "src/pages.Terms",
		"/terms/:date":        "src/pages.Terms",
		"/@:acct/tagged":      "",
		"/@:acct/tagged/:tag": "",
	})
}

// A file that imports RouteObject builds trees a creator elsewhere is given: the arrays
// its functions return are read, a children array is read under its parent only, and
// a nav list of { path, label } is not a router.
func TestReactRouter_RouteObjectTrees(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/pages/Home.tsx": `export function Home() { return null }`,
		"src/routes.tsx": `import { Outlet, RouteObject } from "react-router-dom";
import { Home } from "./pages/Home";
const productChildren = [
  { path: "create", lazy: () => import("./pages/create") },
];
export function getRouteMap({ extra }: { extra: RouteObject[] }) {
  return [
    { element: <Outlet />, children: [
      { path: "/", element: <Home /> },
      { path: "/products", children: [
        { path: "", lazy: () => import("./pages/list"), children: productChildren },
        { path: ":id", lazy: () => import("./pages/detail") },
      ]},
      ...extra,
    ]},
  ];
}
export const nav = [
  { path: "/orders", label: "Orders" },
];`,
	}, false)
	samePages(t, reactRouterPages(ff, "src/routes.tsx"), map[string]string{
		"/":                "src/pages.Home",
		"/products":        "",
		"/products/create": "",
		"/products/:id":    "",
	})
}

func TestOptionalExpansions(t *testing.T) {
	cases := map[string][]string{
		"/a/:b":            {"/a/:b"},
		"/a/:b?":           {"/a", "/a/:b"},
		"/:lang?":          {"/", "/:lang"},
		"/a/?":             nil,
		"/a/x?y":           nil,
		"/:a?/:b?/:c?/:d?": nil,
	}
	for in, want := range cases {
		got := optionalExpansions(in)
		sort.Strings(got)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Errorf("optionalExpansions(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("optionalExpansions(%q) = %v, want %v", in, got, want)
			}
		}
	}
}

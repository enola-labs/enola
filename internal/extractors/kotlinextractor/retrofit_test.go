package kotlinextractor

import (
	"strings"
	"testing"
)

func TestExtractRetrofitFacts(t *testing.T) {
	src := `package com.fairwayhub.mygolfjournal.data.api

interface EntitlementApiService {
    @GET("/api/settings/entitlements/users/{userID}/active")
    suspend fun getActiveEntitlements(@Path("userID") userID: Int): Response<List<ActiveEntitlementDto>>

    @POST("auth/login")
    @Headers("X-Client-Type: mobile")
    suspend fun login(@Body request: LoginRequest): Response<LoginResponse>
}
`
	ff := extractRetrofitFacts([]byte(src), "data/api/EntitlementApiService.kt")
	if len(ff) != 2 {
		t.Fatalf("expected 2 client routes, got %d: %+v", len(ff), ff)
	}

	byName := map[string]map[string]any{}
	for _, f := range ff {
		if f.Kind != "route" {
			t.Errorf("kind = %q, want route", f.Kind)
		}
		if f.Props["role"] != "client" {
			t.Errorf("%s role = %v, want client", f.Name, f.Props["role"])
		}
		if f.Props["framework"] != "retrofit" {
			t.Errorf("%s framework = %v, want retrofit", f.Name, f.Props["framework"])
		}
		if f.Props["api"] != "EntitlementApiService" {
			t.Errorf("%s api hint = %v, want EntitlementApiService", f.Name, f.Props["api"])
		}
		byName[f.Name] = f.Props
	}

	if p, ok := byName["/api/settings/entitlements/users/{userID}/active"]; !ok || p["method"] != "GET" {
		t.Errorf("missing GET entitlements route; got %+v", byName)
	}
	if p, ok := byName["auth/login"]; !ok || p["method"] != "POST" {
		t.Errorf("missing POST auth/login route (relative path); got %+v", byName)
	}
}

// TestRetrofit_AbsoluteURLExternal verifies that a Retrofit annotation with a full
// http(s) URL is tagged external + host (bucketed out of internal coverage) with the
// Name reduced to the base-relative path, while a relative annotation is untouched.
func TestRetrofit_AbsoluteURLExternal(t *testing.T) {
	src := `package com.example.data.api

interface AdService {
    @GET("https://adserver.example.com/track/impression")
    suspend fun track(): Response<Unit>

    @POST("v2/events.json")
    suspend fun events(@Body b: EventBody): Response<Unit>
}
`
	ff := extractRetrofitFacts([]byte(src), "data/api/AdService.kt")
	if len(ff) != 2 {
		t.Fatalf("expected 2 client routes, got %d: %+v", len(ff), ff)
	}
	byName := map[string]map[string]any{}
	for _, f := range ff {
		byName[f.Name] = f.Props
	}

	ext, ok := byName["track/impression"]
	if !ok {
		t.Fatalf("absolute URL should reduce to base-relative path 'track/impression'; got %+v", byName)
	}
	if e, _ := ext["external"].(bool); !e {
		t.Errorf("absolute URL should be external=true; got %+v", ext)
	}
	if ext["host"] != "adserver.example.com" {
		t.Errorf("host = %v, want adserver.example.com", ext["host"])
	}

	rel, ok := byName["v2/events.json"]
	if !ok {
		t.Fatalf("relative route missing; got %+v", byName)
	}
	if _, has := rel["external"]; has {
		t.Errorf("relative annotation must not be external; got %+v", rel)
	}
}

// TestRetrofit_NamedValueArgument covers the `@GET(value = "…")` form.
//
// Kotlin allows any single-argument annotation to be written with its argument
// named, and real Android codebases do — Google's reference app writes every
// endpoint that way. Matching only the positional form yielded ZERO client routes
// for such a repository: not a wrong route, no routes at all, and therefore no
// mobile-to-backend edges and no answer to "which screens break if I change this
// endpoint". Found by adding a production Android app to the benchmark corpus.
func TestRetrofit_NamedValueArgument(t *testing.T) {
	src := `package com.example.core.network.retrofit

interface NetworkApi {
    @GET(value = "topics")
    suspend fun getTopics(@Query("ids") ids: List<String>?): NetworkResponse<List<NetworkTopic>>

    @POST(value = "/api/v1/sync")
    suspend fun sync(@Body body: SyncRequest): NetworkResponse<Unit>

    @GET("positional/still/works")
    suspend fun positional(): NetworkResponse<Unit>
}
`
	ff := extractRetrofitFacts([]byte(src), "core/network/retrofit/NetworkApi.kt")
	if len(ff) != 3 {
		t.Fatalf("expected 3 client routes, got %d: %+v", len(ff), ff)
	}

	byName := map[string]string{}
	for _, f := range ff {
		byName[f.Name], _ = f.Props["method"].(string)
	}
	for path, want := range map[string]string{
		"topics":                 "GET",
		"/api/v1/sync":           "POST",
		"positional/still/works": "GET",
	} {
		if got := byName[path]; got != want {
			t.Errorf("route %q method = %q, want %q (all of: %+v)", path, got, want, byName)
		}
	}
}

// A prefix kept in a constant is substituted into the path; a reference to a
// name the file does not declare is left as written.
func TestRetrofit_ConstantPrefixIsSubstituted(t *testing.T) {
	src := []byte(`package com.shop.orders.api

interface OrderEducationApi {
    @POST("${ROUTE_PREFIX}/read-educations")
    fun markRead(): Single<BaseResponse>

    @GET("$ROUTE_PREFIX/orders/{id}/educations")
    fun educations(@Path("id") id: String): Single<BaseResponse>

    @GET("${OTHER}/elsewhere")
    fun elsewhere(): Single<BaseResponse>

    companion object {
        private const val ROUTE_PREFIX = "orders/api/v2"
    }
}
`)
	var got []string
	for _, f := range extractRetrofitFacts(src, "api/OrderEducationApi.kt") {
		got = append(got, f.Name)
	}
	want := []string{"orders/api/v2/read-educations", "orders/api/v2/orders/{id}/educations", "${OTHER}/elsewhere"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

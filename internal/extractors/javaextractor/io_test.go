package javaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// GAP-JV-02: the Java extractor emits io_direct/performs_io on methods that are
// genuine DB/network round-trips, so the performance analyzer's isExpensiveJvmCall I/O
// index is populated on pure-Java Spring/JPA repos (it was empty — only Kotlin
// carried the prop, from v57). The seed is annotation/interface-driven and
// carries no transitive fixpoint (performs_io == io_direct), mirroring Kotlin's
// actual behavior.

func assertPerformsIO(t *testing.T, ff []facts.Fact, name string) {
	t.Helper()
	f, ok := findFact(ff, name)
	if !ok {
		t.Fatalf("missing symbol %q; got %v", name, names(ff))
	}
	if v, _ := f.Props["io_direct"].(bool); !v {
		t.Errorf("%s io_direct = %v, want true; props=%v", name, f.Props["io_direct"], f.Props)
	}
	if v, _ := f.Props["performs_io"].(bool); !v {
		t.Errorf("%s performs_io = %v, want true; props=%v", name, f.Props["performs_io"], f.Props)
	}
}

func assertNotPerformsIO(t *testing.T, ff []facts.Fact, name string) {
	t.Helper()
	f, ok := findFact(ff, name)
	if !ok {
		t.Fatalf("missing symbol %q; got %v", name, names(ff))
	}
	if _, present := f.Props["io_direct"]; present {
		t.Errorf("%s should not carry io_direct; props=%v", name, f.Props)
	}
	if _, present := f.Props["performs_io"]; present {
		t.Errorf("%s should not carry performs_io; props=%v", name, f.Props)
	}
}

// A @FeignClient interface method is an outbound HTTP round-trip.
func TestJavaIO_FeignClientMethodPerformsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"client/ShippingClient.java": `package client;

import org.springframework.cloud.openfeign.FeignClient;
import org.springframework.web.bind.annotation.GetMapping;

@FeignClient(name = "shipping")
public interface ShippingClient {
    @GetMapping("/api/shipping/{id}")
    Shipment getShipment(String id);
}
`,
	})
	assertPerformsIO(t, ff, "client.ShippingClient.getShipment")
}

// A Spring Data repository interface's declared derived-query method is a DB round-trip.
func TestJavaIO_SpringDataRepositoryMethodPerformsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"repo/UserRepository.java": `package repo;

import org.springframework.data.jpa.repository.JpaRepository;

public interface UserRepository extends JpaRepository<User, Long> {
    User findActiveByTenant(Long tenantId);
}
`,
	})
	assertPerformsIO(t, ff, "repo.UserRepository.findActiveByTenant")
}

// A @Query-annotated method is a custom JPQL/native query — DB I/O — even on an
// interface that is not itself a recognized Spring Data repository base type.
func TestJavaIO_QueryAnnotationPerformsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"repo/WidgetSearch.java": `package repo;

import org.springframework.data.jpa.repository.Query;

public interface WidgetSearch {
    @Query("select w from Widget w where w.name = ?1")
    Widget search(String name);
}
`,
	})
	assertPerformsIO(t, ff, "repo.WidgetSearch.search")
}

// A plain @Service method with an in-memory body performs no I/O.
func TestJavaIO_PlainServiceMethodNotPerformsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"svc/PlainService.java": `package svc;

import org.springframework.stereotype.Service;

@Service
public class PlainService {
    public int compute(int a, int b) {
        return a + b;
    }
}
`,
	})
	assertNotPerformsIO(t, ff, "svc.PlainService.compute")
}

// GUARD (§0-derived): a @RestController @GetMapping method is an INBOUND server
// handler, not an outbound I/O call. It must NOT be flagged, even though it
// carries an HTTP-verb mapping annotation — the "controller handler != I/O" trap
// that reusing Kotlin's bare @GET/@POST set would fall into on server-side Java.
func TestJavaIO_ControllerHandlerNotPerformsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"web/UserController.java": `package web;

import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.bind.annotation.GetMapping;

@RestController
public class UserController {
    @GetMapping("/api/users")
    java.util.List<User> list() {
        return java.util.List.of();
    }
}
`,
	})
	assertNotPerformsIO(t, ff, "web.UserController.list")
}

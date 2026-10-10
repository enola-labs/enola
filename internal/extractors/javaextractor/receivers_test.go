package javaextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func javaSym(t *testing.T, ff []facts.Fact, suffix string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			return f
		}
	}
	t.Fatalf("no symbol ending in %s", suffix)
	return facts.Fact{}
}

func javaCallTargets(f facts.Fact) []string {
	var out []string
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls {
			out = append(out, r.Target)
		}
	}
	return out
}

func javaIO(f facts.Fact) bool {
	b, _ := f.PropAny("performs_io").(bool)
	return b
}

const receiverFixtureRepo = `package com.acme.repo;

import org.springframework.data.jpa.repository.JpaRepository;

public interface UserRepository extends JpaRepository<User, Long> {
    User findByEmail(String email);
}
`

// The dependency-injection chain: controller, service interface, implementation,
// repository. Each hop is a call on a field, and the I/O is at the far end.
func TestJavaCallOnAnInjectedFieldIsAnEdge(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/repo/UserRepository.java": receiverFixtureRepo,
		"src/main/java/com/acme/svc/UserService.java": `package com.acme.svc;

public interface UserService {
    User lookup(String email);
    String label(String email);
}
`,
		"src/main/java/com/acme/svc/UserServiceImpl.java": `package com.acme.svc;

import com.acme.repo.UserRepository;

public class UserServiceImpl implements UserService {
    private final UserRepository userRepository;

    public UserServiceImpl(UserRepository userRepository) {
        this.userRepository = userRepository;
    }

    public User lookup(String email) {
        return userRepository.findByEmail(email);
    }

    public String label(String email) {
        return email.trim();
    }
}
`,
		"src/main/java/com/acme/web/UserController.java": `package com.acme.web;

import com.acme.svc.UserService;
import java.util.List;

public class UserController {
    private final UserService users;

    public UserController(UserService users) {
        this.users = users;
    }

    public void all(List<String> emails) {
        for (String email : emails) {
            users.lookup(email);
        }
    }

    public void labels(List<String> emails) {
        for (String email : emails) {
            users.label(email);
        }
    }
}
`,
	})
	impl := javaSym(t, ff, "UserServiceImpl.lookup")
	if !slices.ContainsFunc(javaCallTargets(impl), func(s string) bool { return strings.HasSuffix(s, "repo.UserRepository.findByEmail") }) {
		t.Errorf("UserServiceImpl.lookup calls %v, want the repository method", javaCallTargets(impl))
	}
	all := javaSym(t, ff, "UserController.all")
	if !slices.ContainsFunc(javaCallTargets(all), func(s string) bool { return strings.HasSuffix(s, "svc.UserService.lookup") }) {
		t.Errorf("UserController.all calls %v, want the service interface's method", javaCallTargets(all))
	}
	// The in-loop list names the method, not the text it was written as.
	if inLoop, _ := all.PropAny("calls_in_loop").([]string); len(inLoop) != 1 || !strings.HasSuffix(inLoop[0], "svc.UserService.lookup") {
		t.Errorf("UserController.all calls_in_loop = %v, want the resolved method", inLoop)
	}
	// And the I/O is carried back along the chain, through the interface.
	if !javaIO(all) {
		t.Errorf("UserController.all is not flagged: controller, service, implementation, repository")
	}
	if javaIO(javaSym(t, ff, "UserController.labels")) {
		t.Errorf("UserController.labels is flagged: its chain ends at a string trim")
	}
	if _, kept := all.Prop("typed_calls"); kept {
		t.Errorf("the walker's note leaked into the facts")
	}
}

// A call on a type this repository does not declare is not an edge, and neither
// is one on a receiver whose type is not written down.
func TestJavaCallOnAnUndeclaredReceiverIsNotAnEdge(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/svc/Report.java": `package com.acme.svc;

import java.util.List;
import java.util.Map;

public class Report {
    private final Map<String, String> names;

    public Report(Map<String, String> names) {
        this.names = names;
    }

    public void run(List<String> keys, Object unknown) {
        for (String key : keys) {
            names.get(key);
            key.trim();
            var v = compute(key);
            v.close();
        }
    }
}
`,
	})
	if got := javaCallTargets(javaSym(t, ff, "Report.run")); len(got) != 0 {
		t.Errorf("Report.run calls %v, want no edge: none of these types is declared here", got)
	}
}

// A method the type inherits is an edge to where it is declared. A local shadows
// a field. A static call names its type.
func TestJavaTypedCallResolvesThroughSupertypesLocalsAndStatics(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/base/AbstractStore.java": `package com.acme.base;

public abstract class AbstractStore {
    public void flush() {}
}
`,
		"src/main/java/com/acme/base/DiskStore.java": `package com.acme.base;

public class DiskStore extends AbstractStore {
    public void write(String s) {}
}
`,
		"src/main/java/com/acme/base/Names.java": `package com.acme.base;

public class Names {
    public static String normalize(String s) { return s; }
}
`,
		"src/main/java/com/acme/job/Job.java": `package com.acme.job;

import com.acme.base.DiskStore;
import com.acme.base.Names;

public class Job {
    private Object store;

    public void run(String s) {
        DiskStore store = new DiskStore();
        store.write(Names.normalize(s));
        store.flush();
    }
}
`,
	})
	got := javaCallTargets(javaSym(t, ff, "Job.run"))
	for _, want := range []string{"base.DiskStore.write", "base.AbstractStore.flush", "base.Names.normalize"} {
		if !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, want) }) {
			t.Errorf("Job.run calls %v, want %s among them", got, want)
		}
	}
}

// A repository inherits its save from the framework. There is no declaration to
// point at, and the receiver's type still says what the call is.
func TestJavaInheritedRepositoryMethodIsNamedAsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/repo/UserRepository.java": receiverFixtureRepo,
		"src/main/java/com/acme/svc/Importer.java": `package com.acme.svc;

import com.acme.repo.UserRepository;
import java.util.List;

public class Importer {
    private final UserRepository userRepository;

    public Importer(UserRepository userRepository) {
        this.userRepository = userRepository;
    }

    public void run(List<User> users) {
        for (User u : users) {
            userRepository.save(u);
        }
    }
}
`,
	})
	run := javaSym(t, ff, "Importer.run")
	if calls, _ := run.PropAny("io_calls").([]string); !slices.Contains(calls, "userRepository.save") {
		t.Errorf("Importer.run io_calls = %v, want userRepository.save", calls)
	}
	if !javaIO(run) {
		t.Errorf("Importer.run is not flagged")
	}
}

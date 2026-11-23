package javaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Spring calls these, never the application: the application class, an advice
// class and its handlers, a configuration's @Bean factories, an aspect's advice.
// Each counts only on the class whose annotation makes Spring call it.

func propOf(t *testing.T, ff []facts.Fact, name, prop string) any {
	t.Helper()
	f, ok := findFact(ff, name)
	if !ok {
		t.Fatalf("missing %q; got %v", name, names(ff))
	}
	return f.Props[prop]
}

func TestSpring_ApplicationAndAdviceClassesAreComponents(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/App.java": `package app;

import org.springframework.boot.autoconfigure.SpringBootApplication;

@SpringBootApplication
public class App {}
`,
		"app/web/Errors.java": `package app.web;

import org.springframework.web.bind.annotation.ControllerAdvice;

@ControllerAdvice
public class Errors {}
`,
		"app/web/RestErrors.java": `package app.web;

import org.springframework.web.bind.annotation.RestControllerAdvice;

@RestControllerAdvice
public class RestErrors {}
`,
	})
	if got := propOf(t, ff, "app.App", "component"); got != "configuration" {
		t.Errorf("App component = %v, want configuration", got)
	}
	for _, n := range []string{"app/web.Errors", "app/web.RestErrors"} {
		if got := propOf(t, ff, n, "component"); got != "controller_advice" {
			t.Errorf("%s component = %v, want controller_advice", n, got)
		}
	}
}

func TestSpring_BeanMethodOfConfigurationIsProvider(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/config/SecurityConfig.java": `package app.config;

import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

@Configuration
public class SecurityConfig {
    @Bean
    public PasswordEncoder passwordEncoder() { return new BCryptPasswordEncoder(); }

    public int helper() { return 1; }
}
`,
		"app/util/Plain.java": `package app.util;

import org.springframework.context.annotation.Bean;

public class Plain {
    @Bean
    public Object notScanned() { return null; }
}
`,
	})
	if got := propOf(t, ff, "app/config.SecurityConfig.passwordEncoder", "di_provider"); got != true {
		t.Errorf("@Bean on @Configuration di_provider = %v, want true", got)
	}
	if got := propOf(t, ff, "app/config.SecurityConfig.helper", "di_provider"); got != nil {
		t.Errorf("plain method di_provider = %v, want none", got)
	}
	if got := propOf(t, ff, "app/util.Plain.notScanned", "di_provider"); got != nil {
		t.Errorf("@Bean on a class Spring does not scan di_provider = %v, want none", got)
	}
}

func TestSpring_ExceptionHandlerOfAdviceIsRegistered(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/web/Errors.java": `package app.web;

import org.springframework.web.bind.annotation.ControllerAdvice;
import org.springframework.web.bind.annotation.ExceptionHandler;

@ControllerAdvice
public class Errors {
    @ExceptionHandler(ApiException.class)
    public Result handle(ApiException e) { return null; }
}
`,
		"app/web/Orders.java": `package app.web;

import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.bind.annotation.ExceptionHandler;

@RestController
public class Orders {
    @ExceptionHandler(IllegalStateException.class)
    public Result local(IllegalStateException e) { return null; }
}
`,
		"app/util/Loose.java": `package app.util;

import org.springframework.web.bind.annotation.ExceptionHandler;

public class Loose {
    @ExceptionHandler(Exception.class)
    public Result never(Exception e) { return null; }
}
`,
	})
	for _, n := range []string{"app/web.Errors.handle", "app/web.Orders.local"} {
		if got := propOf(t, ff, n, "framework_registered"); got != true {
			t.Errorf("%s framework_registered = %v, want true", n, got)
		}
	}
	if got := propOf(t, ff, "app/util.Loose.never", "framework_registered"); got != nil {
		t.Errorf("@ExceptionHandler outside an advice or controller = %v, want none", got)
	}
}

func TestSpring_AspectAdviceIsRegistered(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/log/WebLogAspect.java": `package app.log;

import org.aspectj.lang.annotation.*;
import org.springframework.stereotype.Component;

@Aspect
@Component
public class WebLogAspect {
    @Pointcut("execution(public * app.web.*.*(..))")
    public void webLog() {}

    @Before("webLog()")
    public void doBefore(JoinPoint jp) {}

    @AfterReturning(value = "webLog()", returning = "ret")
    public void doAfterReturning(Object ret) {}

    @Around("webLog()")
    public Object doAround(ProceedingJoinPoint jp) throws Throwable { return jp.proceed(); }

    public void helper() {}
}
`,
		"app/util/NotAnAspect.java": `package app.util;

public class NotAnAspect {
    @Before
    public void setUp() {}
}
`,
	})
	for _, n := range []string{"webLog", "doBefore", "doAfterReturning", "doAround"} {
		if got := propOf(t, ff, "app/log.WebLogAspect."+n, "framework_registered"); got != true {
			t.Errorf("%s framework_registered = %v, want true", n, got)
		}
	}
	if got := propOf(t, ff, "app/log.WebLogAspect.helper", "framework_registered"); got != nil {
		t.Errorf("helper framework_registered = %v, want none", got)
	}
	if got := propOf(t, ff, "app/util.NotAnAspect.setUp", "framework_registered"); got != nil {
		t.Errorf("@Before outside an @Aspect = %v, want none", got)
	}
}

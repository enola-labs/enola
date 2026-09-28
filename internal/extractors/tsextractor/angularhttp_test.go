package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func angularClientRoutes(fs []facts.Fact) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		if f.Kind == facts.KindRoute && f.PropString("api") == "angular-httpclient" {
			out[f.Name] = f.PropString("method")
		}
	}
	return out
}

// The receiver's declared type is what admits a path the general client pass
// rejects: a request path with no leading slash is a path, not a map key.
func TestAngularRelativeRequestPathIsRead(t *testing.T) {
	fs := extractAngular(t, map[string]string{
		"src/app/deploy.service.ts": `import { Injectable } from '@angular/core';
import { HttpClient } from '@angular/common/http';

@Injectable({ providedIn: 'root' })
export class DeployService {
  constructor(private readonly http_: HttpClient) {}

  deploy(spec: unknown) {
    return this.http_.post<string>('api/v1/appdeployment', spec, {});
  }
}
`,
	}, true)

	if got := angularClientRoutes(fs)["/api/v1/appdeployment"]; got != "POST" {
		t.Errorf("relative request path not read; routes: %v", angularClientRoutes(fs))
	}
}

// `await this.http.get(…)` written across lines parses as neither a member call nor
// an error, and every request in one client is written that way.
func TestAngularAwaitedRequestIsRead(t *testing.T) {
	fs := extractAngular(t, map[string]string{
		"src/app/deploy.service.ts": `import { Injectable } from '@angular/core';
import { HttpClient } from '@angular/common/http';

@Injectable({ providedIn: 'root' })
export class DeployService {
  constructor(private readonly http_: HttpClient) {}

  async load() {
    const r = await this.http_
      .get<string>('api/v1/protocols')
      .toPromise();
    return r;
  }
}
`,
	}, true)

	if got := angularClientRoutes(fs)["/api/v1/protocols"]; got != "GET" {
		t.Errorf("awaited request not read; routes: %v", angularClientRoutes(fs))
	}
}

// A base URL is a static of the service that owns the resource and is named by every
// service that touches it, so the constants are only all in hand repo-wide.
func TestAngularRequestPathFromAnotherClassConstant(t *testing.T) {
	fs := extractAngular(t, map[string]string{
		"src/app/video.service.ts": `import { Injectable } from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { environment } from './environment';

@Injectable()
export class VideoService {
  static BASE_VIDEO_URL = environment.apiUrl + '/api/v1/videos';
  private authHttp = inject(HttpClient);
}
`,
		"src/app/live.service.ts": `import { Injectable, inject } from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { VideoService } from './video.service';

@Injectable()
export class LiveService {
  private authHttp = inject(HttpClient);

  getSession(videoId: string) {
    return this.authHttp.get<string>(VideoService.BASE_VIDEO_URL + '/' + videoId + '/live-session');
  }
}
`,
	}, true)

	if got := angularClientRoutes(fs)["/api/v1/videos/{}/live-session"]; got != "GET" {
		t.Errorf("cross-class base URL not folded; routes: %v", angularClientRoutes(fs))
	}
}

// An unresolved LEADING operand means the prefix is unknown, and there is no honest
// way to write the path. It is counted instead.
func TestAngularUnknownBaseIsNotGuessed(t *testing.T) {
	fs := extractAngular(t, map[string]string{
		"src/app/thing.service.ts": `import { Injectable, inject } from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { SomeOtherPackage } from '@acme/lib';

@Injectable()
export class ThingService {
  private http = inject(HttpClient);

  load(id: string) {
    return this.http.get<string>(SomeOtherPackage.BASE + '/things/' + id);
  }
}
`,
	}, true)

	if got := angularClientRoutes(fs); len(got) != 0 {
		t.Errorf("guessed a path with an unknown prefix: %v", got)
	}
	for _, f := range fs {
		if f.Kind == facts.KindExtraction && f.Name == "typescript:angular-requests" {
			if got, _ := f.Props["unresolved_macros"].(string); got != "dynamic_request_path=1" {
				t.Errorf("unresolved causes = %q", got)
			}
			return
		}
	}
	t.Error("no typescript:angular-requests coverage fact")
}

// A receiver that is not a declared HTTP client is not one, whatever it is called.
func TestAngularNonHTTPReceiverIsIgnored(t *testing.T) {
	fs := extractAngular(t, map[string]string{
		"src/app/cache.service.ts": `import { Injectable } from '@angular/core';

@Injectable()
export class CacheService {
  private http = new Map<string, string>();

  read() {
    return this.http.get('/etc/passwd');
  }
}
`,
	}, true)

	if got := angularClientRoutes(fs); len(got) != 0 {
		t.Errorf("read a map lookup as an HTTP request: %v", got)
	}
}

// A spec's traffic is not the application's.
func TestAngularRequestsInTestsAreNotExtracted(t *testing.T) {
	fs := extractAngular(t, map[string]string{
		"src/app/deploy.service.spec.ts": `import { Injectable, inject } from '@angular/core';
import { HttpClient } from '@angular/common/http';

@Injectable()
export class FixtureService {
  private http = inject(HttpClient);
  go() { return this.http.get<string>('api/v1/fixture'); }
}
`,
	}, true)

	if got := angularClientRoutes(fs); len(got) != 0 {
		t.Errorf("a spec's requests were extracted: %v", got)
	}
}

// A request made through an injected HttpClient names the service method making it,
// including from inside an RxJS operator callback, and the method is a symbol the
// declaration walk emitted.
func TestAngularRequestNamesItsCallingMethod(t *testing.T) {
	fs := extractAngular(t, map[string]string{
		"src/app/abuse.service.ts": `import { Injectable } from '@angular/core';
import { HttpClient } from '@angular/common/http';

@Injectable({ providedIn: 'root' })
export class AbuseService {
  constructor(private authHttp: HttpClient) {}

  removeAbuse(ids: number[]) {
    return from(ids).pipe(
      concatMap(id => this.authHttp.delete('/api/v1/abuses/' + id)),
    );
  }

  list = () => this.authHttp.get('/api/v1/abuses');
}
`,
	}, true)
	symbols := map[string]bool{}
	callers := map[string]string{}
	for _, f := range fs {
		if f.Kind == facts.KindSymbol {
			symbols[f.Name] = true
		}
		if f.Kind == facts.KindRoute && f.PropString("api") == "angular-httpclient" {
			callers[f.PropString("method")+" "+f.Name] = f.PropString(facts.PropCaller)
		}
	}
	for route, want := range map[string]string{
		"DELETE /api/v1/abuses/{}": "src/app.AbuseService.removeAbuse",
		"GET /api/v1/abuses":       "src/app.AbuseService.list",
	} {
		if got := callers[route]; got != want || !symbols[got] {
			t.Errorf("%s caller = %q, want the emitted symbol %q (routes: %v)", route, got, want, callers)
		}
	}
}

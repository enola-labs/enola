package rustextractor

import "testing"

// A std::io method called on a value the source shows is an in-memory buffer, or
// on a generic writer or reader parameter, is no I/O of the function's own.
func TestRustIODirect_InMemoryOrGenericReceiverNotIO(t *testing.T) {
	cases := map[string]string{
		"generic bound in type params": `
fn f<W: Write>(krate: &Crate, mut writer: W) {
    writer.write_all(b"\n");
}`,
		"generic bound in where clause": `
fn f<W>(writer: &mut W) where W: std::io::Write {
    writer.write_all(b"\n");
}`,
		"impl Write parameter": `
fn f(writer: &mut impl Write) {
    writer.write_all(b"\n");
}`,
		"generic reader": `
fn f<R: Read>(mut r: R) {
    let mut s = String::new();
    r.read_to_string(&mut s);
}`,
		"Vec::new": `
fn f() {
    let mut body = Vec::new();
    body.write_all(b"x");
}`,
		"Vec::with_capacity": `
fn f(n: usize) {
    let mut body = Vec::with_capacity(n);
    body.write_all(b"x");
}`,
		"vec! macro": `
fn f() {
    let mut body = vec![];
    body.write_all(b"x");
}`,
		"typed let": `
fn f() {
    let mut body: Vec<u8> = make();
    body.write_all(b"x");
}`,
		"String::new": `
fn f(r: &[u8]) {
    let mut s = String::new();
    s.write_all(b"x");
}`,
		"Cursor::new": `
fn f() {
    let mut c = io::Cursor::new(Vec::new());
    c.write_all(b"x");
}`,
		"Vec parameter": `
fn f(buf: &mut Vec<u8>) {
    buf.write_all(b"x");
}`,
		"Cursor parameter": `
fn f(buf: &mut Cursor<Vec<u8>>) {
    buf.write_all(b"x");
}`,
		"byte slice reader": `
fn f(mut data: &[u8]) {
    let mut out = Vec::new();
    data.read_to_end(&mut out);
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f, ok := findFact(extractAST(t, src), "pkg.f")
			if !ok {
				t.Fatal("pkg.f not extracted")
			}
			if b, _ := f.Props["io_direct"].(bool); b {
				t.Errorf("io_direct = true, want false: the receiver is an in-memory buffer or a generic parameter")
			}
		})
	}
}

// The same methods on a receiver the source does not show to be a buffer, or
// shows to be a real sink, stay direct I/O.
func TestRustIODirect_RealSinkReceiverStillIO(t *testing.T) {
	cases := map[string]string{
		"File parameter": `
fn f(file: &mut File) {
    file.write_all(b"x");
}`,
		"TcpStream parameter": `
fn f(stream: &mut TcpStream) {
    stream.write_all(b"x");
}`,
		"stdout call": `
fn f() {
    std::io::stdout().write_all(b"x");
}`,
		"stdout lock local": `
fn f() {
    let mut out = io::stdout().lock();
    out.write_all(b"x");
}`,
		"dyn Write parameter": `
fn f(out: &mut dyn Write) {
    out.write_all(b"x");
}`,
		"untyped local": `
fn f(addr: &str) {
    let mut s = connect(addr);
    s.write_all(b"x");
}`,
		"buffer shadowed by a socket": `
fn f(addr: &str) {
    let mut s = Vec::new();
    let mut s = connect(addr);
    s.write_all(b"x");
}`,
		"generic executor is a database call": `
fn f<E: Executor>(db: E) {
    db.execute("select 1");
}`,
		"generic sender": `
fn f<C: Client>(client: C) {
    client.send();
}`,
		"inner fn does not see outer buffer": `
fn g() {
    let mut out = Vec::new();
    out.write_all(b"x");
}
fn f(out: &mut File) {
    out.write_all(b"x");
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			f, ok := findFact(extractAST(t, src), "pkg.f")
			if !ok {
				t.Fatal("pkg.f not extracted")
			}
			if b, _ := f.Props["io_direct"].(bool); !b {
				t.Errorf("io_direct = %v, want true: the receiver is not shown to be a buffer", f.Props["io_direct"])
			}
		})
	}
}

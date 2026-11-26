package orphans

import (
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/facts"
)

// Swift marks no protocol witness with a keyword. A member that satisfies a
// requirement of a protocol its type conforms to is called through the protocol:
// by synthesized Codable, a for-in loop, an == operator, UIKit sending a delegate
// message, a metrics backend. Nothing names it, so it read as dead.
//
// A protocol the repository does not declare cannot be read, so its requirements
// come from a table, and a member is a witness only when both halves hold: its
// type, or an extension of its type, names the protocol in its conformance list,
// and the protocol declares a requirement of the member's name. Conforming to
// Sendable or Equatable roots nothing but ==, and a member of an encoding
// container that no container protocol declares is still reported.
//
// Cocoa delegate and data source protocols are too many to list, and they follow
// one convention instead: each requirement is named after the delegating object,
// which the protocol is named after. UITableViewDelegate's are tableView(_:…),
// UIApplicationDelegate's application(_:…) and applicationDidBecomeActive(_:). A
// member of a type conforming to such a protocol is a witness when its name is
// that stem, or the stem followed by a capitalized word.
//
// A protocol the repository declares is never read from the table: its
// requirements are the repository's, and calls through it are recorded.

// swiftProtocolRequirements names, per protocol a Swift repository conforms to but
// cannot declare, the requirements its callers invoke by the protocol.
var swiftProtocolRequirements = map[string]map[string]bool{
	// Standard library.
	"Equatable":                      methodSet("=="),
	"Hashable":                       methodSet("==", "hash"),
	"Comparable":                     methodSet("==", "<", "<=", ">", ">="),
	"Sequence":                       methodSet("makeIterator", "underestimatedCount", "withContiguousStorageIfAvailable"),
	"IteratorProtocol":               methodSet("next"),
	"AsyncSequence":                  methodSet("makeAsyncIterator"),
	"AsyncIteratorProtocol":          methodSet("next"),
	"Collection":                     methodSet("makeIterator", "index", "formIndex", "distance"),
	"BidirectionalCollection":        methodSet("makeIterator", "index", "formIndex", "distance"),
	"RandomAccessCollection":         methodSet("makeIterator", "index", "formIndex", "distance"),
	"MutableCollection":              methodSet("makeIterator", "index", "formIndex", "distance", "partition", "swapAt"),
	"RangeReplaceableCollection":     methodSet("makeIterator", "index", "formIndex", "distance", "replaceSubrange", "reserveCapacity", "append", "insert", "remove", "removeSubrange", "removeAll"),
	"Encodable":                      methodSet("encode"),
	"Codable":                        methodSet("encode"),
	"Encoder":                        methodSet("container", "unkeyedContainer", "singleValueContainer"),
	"Decoder":                        methodSet("container", "unkeyedContainer", "singleValueContainer"),
	"KeyedEncodingContainerProtocol": methodSet("encodeNil", "encode", "encodeIfPresent", "encodeConditional", "nestedContainer", "nestedUnkeyedContainer", "superEncoder"),
	"UnkeyedEncodingContainer":       methodSet("encodeNil", "encode", "encodeConditional", "nestedContainer", "nestedUnkeyedContainer", "superEncoder"),
	"SingleValueEncodingContainer":   methodSet("encodeNil", "encode"),
	"KeyedDecodingContainerProtocol": methodSet("contains", "decodeNil", "decode", "decodeIfPresent", "nestedContainer", "nestedUnkeyedContainer", "superDecoder"),
	"UnkeyedDecodingContainer":       methodSet("decodeNil", "decode", "decodeIfPresent", "nestedContainer", "nestedUnkeyedContainer", "superDecoder"),
	"SingleValueDecodingContainer":   methodSet("decodeNil", "decode"),
	"TextOutputStream":               methodSet("write"),
	"TextOutputStreamable":           methodSet("write"),
	// Foundation.
	"FormatStyle":          methodSet("format", "locale"),
	"ParseableFormatStyle": methodSet("format", "locale"),
	"ParseStrategy":        methodSet("parse"),
	"NSCopying":            methodSet("copy"),
	"NSCoding":             methodSet("encode"),
	// SwiftUI.
	"ViewModifier":                  methodSet("body"),
	"ButtonStyle":                   methodSet("makeBody"),
	"PrimitiveButtonStyle":          methodSet("makeBody"),
	"ToggleStyle":                   methodSet("makeBody"),
	"LabelStyle":                    methodSet("makeBody"),
	"ProgressViewStyle":             methodSet("makeBody"),
	"GroupBoxStyle":                 methodSet("makeBody"),
	"MenuStyle":                     methodSet("makeBody"),
	"Shape":                         methodSet("path", "sizeThatFits"),
	"InsettableShape":               methodSet("path", "sizeThatFits", "inset"),
	"GeometryEffect":                methodSet("effectValue"),
	"PreferenceKey":                 methodSet("reduce"),
	"DynamicProperty":               methodSet("update"),
	"Layout":                        methodSet("sizeThatFits", "placeSubviews", "makeCache", "updateCache", "spacing", "explicitAlignment"),
	"UIViewRepresentable":           methodSet("makeUIView", "updateUIView", "makeCoordinator", "dismantleUIView", "sizeThatFits"),
	"UIViewControllerRepresentable": methodSet("makeUIViewController", "updateUIViewController", "makeCoordinator", "dismantleUIViewController", "sizeThatFits"),
	"NSViewRepresentable":           methodSet("makeNSView", "updateNSView", "makeCoordinator", "dismantleNSView", "sizeThatFits"),
	"NSViewControllerRepresentable": methodSet("makeNSViewController", "updateNSViewController", "makeCoordinator", "dismantleNSViewController", "sizeThatFits"),
	// UIKit and its neighbours, where a requirement does not follow the delegate
	// convention below.
	"UIViewControllerAnimatedTransitioning":                 methodSet("animateTransition", "transitionDuration", "animationEnded", "interruptibleAnimator"),
	"UIViewControllerTransitioningDelegate":                 methodSet("animationController", "presentationController", "interactionControllerForPresentation", "interactionControllerForDismissal"),
	"UITableViewDataSource":                                 methodSet("numberOfSections", "sectionIndexTitles"),
	"UICollectionViewDataSource":                            methodSet("numberOfSections", "indexTitles"),
	"UIPickerViewDataSource":                                methodSet("numberOfComponents"),
	"UIPageViewControllerDataSource":                        methodSet("presentationCount", "presentationIndex"),
	"MFMailComposeViewControllerDelegate":                   methodSet("mailComposeController"),
	"MFMessageComposeViewControllerDelegate":                methodSet("messageComposeViewController"),
	"SKPaymentTransactionObserver":                          methodSet("paymentQueue", "paymentQueueRestoreCompletedTransactionsFinished"),
	"WKNavigationDelegate":                                  methodSet("webView", "webViewWebContentProcessDidTerminate"),
	"WKUIDelegate":                                          methodSet("webView", "webViewDidClose"),
	"WKScriptMessageHandler":                                methodSet("userContentController"),
	"WKExtensionDelegate":                                   methodSet("applicationDidFinishLaunching", "applicationDidBecomeActive", "applicationWillResignActive", "handle"),
	"ASAuthorizationControllerPresentationContextProviding": methodSet("presentationAnchor"),
	"ASWebAuthenticationPresentationContextProviding":       methodSet("presentationAnchor"),
	// Server-side Swift packages: swift-metrics, swift-distributed-tracing,
	// swift-service-lifecycle, swift-testing traits.
	"MetricsFactory": methodSet("makeCounter", "makeFloatingPointCounter", "makeMeter", "makeRecorder", "makeTimer",
		"destroyCounter", "destroyFloatingPointCounter", "destroyMeter", "destroyRecorder", "destroyTimer"),
	"Instrument":               methodSet("extract", "inject"),
	"LegacyTracer":             methodSet("extract", "inject", "startAnySpan", "forceFlush"),
	"Tracer":                   methodSet("extract", "inject", "startAnySpan", "startSpan", "forceFlush", "activeSpan"),
	"Service":                  methodSet("run"),
	"TestScoping":              methodSet("provideScope"),
	"TestTrait":                methodSet("prepare", "scopeProvider"),
	"SuiteTrait":               methodSet("prepare", "scopeProvider"),
	"HTTPServerRequestHandler": methodSet("handle"),
}

// swiftAssociatedTypes names, per protocol a Swift repository conforms to but cannot
// declare, its associated types. A type nested in a conforming type under one of
// these names witnesses it: the protocol's users name it through the conformance
// (`S.Element`, `Publisher.Failure`, `StoreOf<R>` is `Store<R.State, R.Action>`),
// so nothing names it directly. The Composable Architecture's @Reducer macro adds
// the Reducer conformance the extractor records.
var swiftAssociatedTypes = map[string]map[string]bool{
	"Sequence":                   methodSet("Element", "Iterator"),
	"AsyncSequence":              methodSet("Element", "AsyncIterator", "Failure"),
	"IteratorProtocol":           methodSet("Element"),
	"AsyncIteratorProtocol":      methodSet("Element", "Failure"),
	"Collection":                 methodSet("Element", "Index", "Indices", "Iterator", "SubSequence"),
	"BidirectionalCollection":    methodSet("Element", "Index", "Indices", "Iterator", "SubSequence"),
	"RandomAccessCollection":     methodSet("Element", "Index", "Indices", "Iterator", "SubSequence"),
	"MutableCollection":          methodSet("Element", "Index", "Indices", "Iterator", "SubSequence"),
	"RangeReplaceableCollection": methodSet("Element", "Index", "Indices", "Iterator", "SubSequence"),
	"Publisher":                  methodSet("Output", "Failure"),
	"Subscriber":                 methodSet("Input", "Failure"),
	"View":                       methodSet("Body"),
	"ViewModifier":               methodSet("Body"),
	"Scene":                      methodSet("Body"),
	"App":                        methodSet("Body"),
	"ParseStrategy":              methodSet("ParseInput", "ParseOutput"),
	"FormatStyle":                methodSet("FormatInput", "FormatOutput"),
	"ParseableFormatStyle":       methodSet("FormatInput", "FormatOutput", "Strategy"),
	"Reducer":                    methodSet("State", "Action"),
	// SwiftUI's representables: the wrapped view or controller, and the
	// coordinator makeCoordinator returns.
	"UIViewRepresentable":           methodSet("UIViewType", "Coordinator"),
	"UIViewControllerRepresentable": methodSet("UIViewControllerType", "Coordinator"),
	"NSViewRepresentable":           methodSet("NSViewType", "Coordinator"),
	"NSViewControllerRepresentable": methodSet("NSViewControllerType", "Coordinator"),
	// swift-http-api's request handler.
	"HTTPServerRequestHandler": methodSet("RequestContext", "Reader", "ResponseSender"),
	// MultipartKit: the part body a type converts to and from.
	"MultipartPartConvertible": methodSet("Body"),
	"MultipartPartEncodable":   methodSet("Body"),
	"MultipartPartDecodable":   methodSet("Body"),
}

// swiftDelegateInherits adds the stems of the delegate protocols a protocol
// refines: a table view's delegate is its scroll view's delegate, a window
// scene's delegate is a scene's, and every URLSession delegate's requirements are
// urlSession(_:…), whatever the task kind its protocol is named after.
var swiftDelegateInherits = map[string][]string{
	"URLSessionTaskDelegate":             {"urlSession"},
	"URLSessionDataDelegate":             {"urlSession"},
	"URLSessionDownloadDelegate":         {"urlSession"},
	"URLSessionStreamDelegate":           {"urlSession"},
	"URLSessionWebSocketDelegate":        {"urlSession"},
	"UITableViewDelegate":                {"scrollView"},
	"UICollectionViewDelegate":           {"scrollView"},
	"UICollectionViewDelegateFlowLayout": {"scrollView", "collectionView"},
	"UITextViewDelegate":                 {"scrollView"},
	"UIWindowSceneDelegate":              {"scene"},
}

// swiftDelegateSuffixes end the name of a protocol that follows the delegate
// convention.
var swiftDelegateSuffixes = []string{"DelegateFlowLayout", "DataSourcePrefetching", "DataSource", "Delegate"}

// swiftPreviewProtocols are found by Xcode by conformance alone, so a type that
// conforms is entered by the tool, as a Kotlin @Preview function is.
var swiftPreviewProtocols = map[string]bool{"PreviewProvider": true}

// swiftWitnesses returns the Swift methods that witness a requirement of a protocol
// the repository does not declare, and the types Xcode enters by conformance.
func swiftWitnesses(symbols []facts.Fact) (witnesses, entered map[string]bool) {
	declared := map[string]bool{}
	conforms := map[[2]string][]string{} // (repo, type path within its module) -> protocols
	for i := range symbols {
		f := &symbols[i]
		if f.PropString(propLanguage) != "swift" {
			continue
		}
		kind := f.PropString(propSymbolKind)
		if kind == facts.SymbolInterface {
			declared[lastDotComponent(f.Name)] = true
		}
		if kind != facts.SymbolClass && kind != facts.SymbolStruct && kind != facts.SymbolExtension && kind != facts.SymbolInterface {
			continue
		}
		path := swiftTypePath(f)
		if path == "" {
			continue
		}
		if kind == facts.SymbolExtension {
			path, _, _ = strings.Cut(path, "+")
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				key := [2]string{f.Repo, path}
				conforms[key] = append(conforms[key], lastDotComponent(r.Target))
			}
		}
	}
	witnesses, entered = map[string]bool{}, map[string]bool{}
	for i := range symbols {
		f := &symbols[i]
		if f.PropString(propLanguage) != "swift" {
			continue
		}
		switch kind := f.PropString(propSymbolKind); kind {
		case facts.SymbolClass, facts.SymbolStruct, facts.SymbolType:
			path := swiftTypePath(f)
			for _, p := range conforms[[2]string{f.Repo, path}] {
				if kind != facts.SymbolType && swiftPreviewProtocols[p] && !declared[p] {
					entered[f.Name] = true
				}
			}
			if dot := strings.LastIndexByte(path, '.'); dot > 0 {
				name := path[dot+1:]
				for _, p := range conforms[[2]string{f.Repo, path[:dot]}] {
					if !declared[p] && swiftAssociatedTypes[p][name] {
						witnesses[f.Name] = true
						break
					}
				}
			}
		case facts.SymbolMethod:
			recv := f.PropString("receiver")
			if recv == "" {
				continue
			}
			name := lastDotComponent(f.Name)
			for _, p := range conforms[[2]string{f.Repo, recv}] {
				if !declared[p] && swiftRequires(p, name) {
					witnesses[f.Name] = true
					break
				}
			}
		}
	}
	return witnesses, entered
}

// swiftTypePath is a type's name within its module: the fact's name without the
// module it declares itself in ("_Encoder.KeyedContainer").
func swiftTypePath(f *facts.Fact) string {
	for _, r := range f.Relations {
		if r.Kind == facts.RelDeclares {
			if rest, ok := strings.CutPrefix(f.Name, r.Target+"."); ok {
				return rest
			}
		}
	}
	return ""
}

func lastDotComponent(name string) string {
	return name[strings.LastIndexByte(name, '.')+1:]
}

// swiftRequires reports whether protocol p, which the repository does not declare,
// has a requirement named name.
func swiftRequires(p, name string) bool {
	if swiftProtocolRequirements[p][name] {
		return true
	}
	stems := swiftDelegateInherits[p]
	if s := swiftDelegateStem(p); s != "" {
		stems = append(stems, s)
	}
	for _, s := range stems {
		if rest, ok := strings.CutPrefix(name, s); ok && (rest == "" || unicode.IsUpper(rune(rest[0]))) {
			return true
		}
	}
	return false
}

// swiftDelegateBase is the delegating object's type named by a delegate
// protocol: UITableViewDelegate -> UITableView. It is "" for a protocol that does
// not follow the convention.
func swiftDelegateBase(p string) string {
	for _, suf := range swiftDelegateSuffixes {
		if base, ok := strings.CutSuffix(p, suf); ok && base != "" {
			return base
		}
	}
	return ""
}

// swiftDelegateStem is the requirement stem of a delegate protocol: the
// delegating type with a two-letter framework prefix dropped (UITableView ->
// tableView, UNUserNotificationCenter -> userNotificationCenter) or, with none, its
// leading capitals lowered (URLSession -> urlSession).
func swiftDelegateStem(p string) string {
	base := swiftDelegateBase(p)
	upper := 0
	for upper < len(base) && base[upper] >= 'A' && base[upper] <= 'Z' {
		upper++
	}
	switch {
	case upper == 0 || upper == len(base):
		return ""
	case upper == 3:
		return strings.ToLower(base[2:3]) + base[3:]
	case upper == 1:
		return strings.ToLower(base[:1]) + base[1:]
	}
	return strings.ToLower(base[:upper-1]) + base[upper-1:]
}

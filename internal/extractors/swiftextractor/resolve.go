package swiftextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// resolveImports rewrites, in place, Swift `import X` dependency facts whose
// relation target is a bare module name, and sets Props["source"] on every
// dependency fact.
//
// Swift imports name a module (an SPM target or a system framework), not a path,
// so handleImport emits the bare name ("import AppComposition" -> "AppComposition")
// with no source. That never matches a module fact Name (a slash dir), so coupling
// collapses. SPM target module facts carry Props["spm_target"] and are named by
// their Sources directory, so this pass maps a bare import name to that dir and
// classifies the rest as stdlib (Apple system frameworks) or external.
func resolveImports(allFacts []facts.Fact) {
	// Module name -> module dir, keyed by the target's importable name. Swift
	// `import X` names an SPM target (spm_target) or an XcodeGen framework target
	// (xcode_target); both map back to the module's Sources directory.
	spmDir := make(map[string]string)
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindModule {
			continue
		}
		if name, ok := f.PropAny("spm_target").(string); ok && name != "" {
			spmDir[name] = f.Name
		}
		if name, ok := f.PropAny("xcode_target").(string); ok && name != "" {
			spmDir[name] = f.Name
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindDependency {
			continue
		}
		for j := range f.Relations {
			rel := &f.Relations[j]
			if rel.Kind != facts.RelImports {
				continue
			}
			t := rel.Target

			// Targets that are already a path come from manifest parsing or the
			// type-reference pass; leave them, just normalise source.
			if strings.Contains(t, "/") || t == "." {
				setSource(f, sourceForResolvedDep(f))
				continue
			}

			// Bare module name: resolve to an SPM target dir, or classify.
			switch {
			case spmDir[t] != "":
				rel.Target = spmDir[t]
				setSource(f, "internal")
			case swiftSystemFramework[t]:
				setSource(f, "stdlib")
			default:
				setSource(f, "external")
			}
		}
	}
}

// sourceForResolvedDep returns the source label for a dependency fact whose
// target is already a path: "internal" when it was flagged so by the
// type-reference pass or carries an internal source, else the existing/external.
func sourceForResolvedDep(f *facts.Fact) string {
	if s, ok := f.PropAny("source").(string); ok && s != "" {
		return s
	}
	if b, _ := f.PropAny("internal").(bool); b {
		return "internal"
	}
	return "internal" // a path target inside the repo is internal by construction
}

// setSource sets Props["source"] (overwriting only when empty/unset).
func setSource(f *facts.Fact, source string) {
	if f.Props == nil {
		f.Props = map[string]any{}
	}
	if s, ok := f.PropAny("source").(string); ok && s != "" {
		return
	}
	f.SetProp("source", source)
}

// swiftSystemFramework is the set of Apple/system module names that an `import`
// can name. Used to split non-internal imports into "stdlib" vs "external"
// (third-party SPM/CocoaPods deps).
var swiftSystemFramework = map[string]bool{
	"Swift": true, "Foundation": true, "Combine": true, "Dispatch": true,
	"os": true, "OSLog": true, "Darwin": true, "ObjectiveC": true, "simd": true,
	"Observation": true, "SwiftData": true,
	// UI
	"UIKit": true, "SwiftUI": true, "SwiftUICore": true, "AppKit": true,
	"WatchKit": true, "WidgetKit": true, "Charts": true, "WebKit": true,
	"SafariServices": true, "MessageUI": true, "PDFKit": true, "QuickLook": true,
	"QuickLookThumbnailing": true, "UserNotifications": true, "UserNotificationsUI": true,
	"PhotosUI": true, "QuartzCore": true, "CoreAnimation": true,
	// Core
	"CoreData": true, "CoreGraphics": true, "CoreLocation": true, "CoreFoundation": true,
	"CoreImage": true, "CoreMedia": true, "CoreText": true, "CoreBluetooth": true,
	"CoreMotion": true, "CoreML": true, "CoreAudio": true, "CoreTelephony": true,
	"CoreSpotlight": true, "CoreHaptics": true, "CoreVideo": true, "CoreServices": true,
	// Media / graphics
	"AVFoundation": true, "AVKit": true, "MediaPlayer": true, "ImageIO": true,
	"Metal": true, "MetalKit": true, "ModelIO": true, "SpriteKit": true,
	"SceneKit": true, "ARKit": true, "RealityKit": true, "Vision": true,
	"VideoToolbox": true, "Photos": true,
	// Services / data
	"MapKit": true, "StoreKit": true, "CloudKit": true, "Network": true,
	"Security": true, "LocalAuthentication": true, "AuthenticationServices": true,
	"Contacts": true, "ContactsUI": true, "EventKit": true, "EventKitUI": true,
	"HealthKit": true, "HomeKit": true, "GameKit": true, "Intents": true,
	"IntentsUI": true, "CallKit": true, "PushKit": true, "BackgroundTasks": true,
	"GroupActivities": true, "NaturalLanguage": true, "Speech": true,
	"Accelerate": true, "MetricKit": true, "DeviceCheck": true, "AdSupport": true,
	"AppTrackingTransparency": true, "LinkPresentation": true, "UniformTypeIdentifiers": true,
	// Testing
	"XCTest": true, "Testing": true,
}

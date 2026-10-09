package javaextractor

// GAP-JV-02 — Java I/O identity.
//
// A method carries io_direct (and performs_io) when it is a genuine DB/network
// round-trip. The methods that reach one get performs_io from ioclosure.Propagate.
// The Kotlin extractor keys off bare HTTP-verb annotations (@GET/@POST) because on
// Android those only ever mean a Retrofit client call. That set is UNSAFE on
// server-side Java, where @GET is a JAX-RS inbound resource and @GetMapping a
// Spring controller handler — neither is an outbound I/O call. So the Java seed
// uses type-level discriminators (a @FeignClient interface, a Spring Data
// repository interface, a Room @Dao interface) plus query annotations that have no
// server-handler meaning.

// javaQueryAnnotation reports a custom JPQL / native / stored-procedure query
// method: Spring Data @Query / @Modifying / @Procedure. Safe anywhere.
func javaQueryAnnotation(annotations []javaAnnotation) bool {
	return hasAnnotation(annotations, "Query", "Modifying", "Procedure")
}

// javaRoomOp reports a Room DAO operation. Honored only inside a @Dao interface
// (routeScope.isDao) so it cannot collide with a JAX-RS @Query/@Delete elsewhere.
func javaRoomOp(annotations []javaAnnotation) bool {
	return hasAnnotation(annotations, "Query", "Insert", "Update", "Delete", "Upsert", "RawQuery")
}

// methodPerformsIO decides whether the method currently being emitted is a direct
// DB/network round-trip, given its own annotations and its enclosing type scope.
func (w *astWalker) methodPerformsIO(annotations []javaAnnotation) bool {
	if javaQueryAnnotation(annotations) {
		return true
	}
	rs := w.currentRoute()
	if rs == nil {
		return false
	}
	switch {
	case rs.isFeignClient: // every method is an outbound HTTP call
		return true
	case rs.isRepository: // every declared derived-query method is a DB round-trip
		return true
	case rs.isDao && javaRoomOp(annotations): // Room SQL op inside a @Dao interface
		return true
	}
	return false
}

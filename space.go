package physics

import (
	"math"
	"unsafe"
)

const MAX_CONTACTS_PER_ARBITER = 2
const CONTACTS_BUFFER_SIZE = 1024

type Space struct {
	Iterations uint // must be non-zero

	gravity Vector
	damping float64

	idleSpeedThreshold float64
	SleepTimeThreshold float64

	collisionSlop        float64
	collisionBias        float64
	collisionPersistence uint

	userData interface{}

	stamp   uint
	curr_dt float64

	dynamicBodies      []*Body
	staticBodies       []*Body
	rousedBodies       []*Body
	sleepingComponents []*Body

	shapeIDCounter uint
	staticShapes   *SpatialIndex
	dynamicShapes  *SpatialIndex

	constraints []*Constraint

	arbiters           []*Arbiter
	contactBuffersHead *ContactBuffer
	cachedArbiters     *HashSetArbiter
	pooledArbiters     chan *Arbiter

	locked int

	usesWildcards     bool
	collisionHandlers *HashSetCollisionHandler
	defaultHandler    *CollisionHandler

	skipPostStep      bool
	postStepCallbacks []PostStepCallback

	StaticBody *Body
}

func arbiterSetEql(shapes []*Shape, arb *Arbiter) bool {
	a := shapes[0]
	b := shapes[1]

	return (a == arb.a && b == arb.b) || (b == arb.a && a == arb.b)
}

func handlerSetTrans(handler, _ interface{}) interface{} {
	// Chipmunk makes a copy, not sure that is possible in Go
	return handler
}

func NewSpace() *Space {
	space := &Space{
		Iterations:           10,
		gravity:              VectorZero(),
		damping:              1.0,
		collisionSlop:        0.1,
		collisionBias:        math.Pow(0.9, 60),
		collisionPersistence: 3,
		locked:               0,
		stamp:                0,
		shapeIDCounter:       0,
		staticShapes:         NewBBTree(ShapeGetBB, nil),
		dynamicBodies:        []*Body{},
		staticBodies:         []*Body{},
		sleepingComponents:   []*Body{},
		rousedBodies:         []*Body{},
		SleepTimeThreshold:   math.MaxFloat64,
		idleSpeedThreshold:   0.0,
		arbiters:             []*Arbiter{},
		cachedArbiters:       NewHashSetArbiter(arbiterSetEql),
		pooledArbiters:       make(chan *Arbiter, POOLED_BUFFER_SIZE),
		constraints:          []*Constraint{},
		collisionHandlers:    NewHashSetCollisionHandler(),
		postStepCallbacks:    []PostStepCallback{},
		defaultHandler:       &CollisionHandlerDoNothing,
	}
	space.dynamicShapes = NewBBTree(ShapeGetBB, space.staticShapes)
	space.dynamicShapes.class.(*BBTree).velocityFunc = BBTreeVelocityFunc(ShapeVelocityFunc)
	staticBody := NewBody(0, 0)
	staticBody.SetType(BODY_STATIC)
	space.SetStaticBody(staticBody)
	return space
}

var ShapeVelocityFunc = func(obj interface{}) Vector {
	return obj.(*Shape).body.v
}

func (space *Space) SetGravity(gravity Vector) {
	space.gravity = gravity

	// Wake up all of the bodies since the gravity changed.
	for _, component := range space.sleepingComponents {
		component.Activate()
	}
}

func (space *Space) SetCollisionSlop(slop float64) {
	space.collisionSlop = slop
}

func (space *Space) SetStaticBody(body *Body) {
	if space.StaticBody != nil {
		space.StaticBody.space = nil
		panic("Internal Error: Changing the designated static body while the old one still had shapes attached.")
	}
	space.StaticBody = body
	body.space = space
}

func (space *Space) Activate(body *Body) {
	assert(body.GetType() == BODY_DYNAMIC, "Attempting to activate a non-dynamic body")

	if space.locked != 0 {
		if !Contains(space.rousedBodies, body) {
			space.rousedBodies = append(space.rousedBodies, body)
		}
		return
	}

	assert(body.sleepingRoot == nil && body.sleepingNext == nil, "Activating body non-NULL node pointers.")

	space.dynamicBodies = append(space.dynamicBodies, body)

	for _, shape := range body.shapeList {
		space.staticShapes.class.Remove(shape, shape.hashid)
		space.dynamicShapes.class.Insert(shape, shape.hashid)
	}

	for arbiter := body.arbiterList; arbiter != nil; arbiter = arbiter.Next(body) {
		bodyA := arbiter.body_a

		// Arbiters are shared between two bodies that are always woken up together.
		// You only want to restore the arbiter once, so bodyA is arbitrarily chosen to own the arbiter.
		// The edge case is when static bodies are involved as the static bodies never actually sleep.
		// If the static body is bodyB then all is good. If the static body is bodyA, that can easily be checked.
		if body == bodyA || bodyA.GetType() == BODY_STATIC {
			numContacts := arbiter.count
			contacts := arbiter.contacts

			// Restore contact values back to the space's contact buffer memory
			arbiter.contacts = space.ContactBufferGetArray()[:numContacts]
			for i, contact := range contacts {
				arbiter.contacts[i] = contact
			}
			space.PushContacts(numContacts)

			// reinsert the arbiter into the arbiter cache
			a := arbiter.a
			b := arbiter.b
			shapePair := []*Shape{a, b}
			arbHashId := HashPair(HashValue(unsafe.Pointer(a)), HashValue(unsafe.Pointer(b)))
			space.cachedArbiters.InsertArb(arbHashId, shapePair, arbiter)

			// update arbiters state
			arbiter.stamp = space.stamp
			space.arbiters = append(space.arbiters, arbiter)
		}
	}

	for constraint := body.constraintList; constraint != nil; constraint = constraint.Next(body) {
		if body == constraint.a || constraint.a.GetType() == BODY_STATIC {
			space.constraints = append(space.constraints, constraint)
		}
	}
}

func (space *Space) Deactivate(body *Body) {
	assert(body.GetType() == BODY_DYNAMIC, "Attempting to deactivate non-dynamic body.")

	for i, v := range space.dynamicBodies {
		if v == body {
			space.dynamicBodies = append(space.dynamicBodies[:i], space.dynamicBodies[i+1:]...)
			break
		}
	}

	for _, shape := range body.shapeList {
		space.dynamicShapes.class.Remove(shape, shape.hashid)
		space.staticShapes.class.Insert(shape, shape.hashid)
	}

	for arb := body.arbiterList; arb != nil; arb = ArbiterNext(arb, body) {
		bodyA := arb.body_a
		if body == bodyA || bodyA.GetType() == BODY_STATIC {
			space.UncacheArbiter(arb)

			// Save contact values to a new block of memory so they won't time out
			contacts := arb.contacts
			arb.contacts = []*Contact{}
			for _, c := range contacts {
				arb.contacts = append(arb.contacts, c.Clone())
			}
		}
	}

	for constraint := body.constraintList; constraint != nil; constraint = constraint.Next(body) {
		bodyA := constraint.a
		if body == bodyA || bodyA.GetType() == BODY_STATIC {
			for i, c := range space.constraints {
				if c == constraint {
					space.constraints = append(space.constraints[0:i], space.constraints[i+1:]...)
				}
			}
		}
	}
}

type PostStepCallback struct {
	callback PostStepCallbackFunc
	key      interface{}
	data     interface{}
}

type PostStepCallbackFunc func(space *Space, key interface{}, data interface{})

func Contains(bodies []*Body, body *Body) bool {
	for i := 0; i < len(bodies); i++ {
		if bodies[i] == body {
			return true
		}
	}
	return false
}

func (space *Space) AddShape(shape *Shape) *Shape {
	var body *Body = shape.Body()

	assert(shape.space != space, "You have already added this shape to this space. You must not add it a second time.")
	assert(shape.space == nil, "You have already added this shape to another space. You cannot add it to a second.")
	assert(space.locked == 0, "This operation cannot be done safely during a call to cpSpaceStep() or during a query. Put these calls into a post-step callback.")

	isStatic := body.GetType() == BODY_STATIC
	if !isStatic {
		body.Activate()
	}
	body.AddShape(shape)

	shape.SetHashId(HashValue(space.shapeIDCounter))
	space.shapeIDCounter += 1
	shape.Update(body.transform)

	if isStatic {
		space.staticShapes.class.Insert(shape, shape.HashId())
	} else {
		space.dynamicShapes.class.Insert(shape, shape.HashId())
	}
	shape.SetSpace(space)

	return shape
}

func (space *Space) AddBody(body *Body) *Body {
	assert(body.space != space, "Already added to this space")
	assert(body.space == nil, "Already added to another space")
	if body.GetType() == BODY_STATIC {
		space.staticBodies = append(space.staticBodies, body)
	} else {
		space.dynamicBodies = append(space.dynamicBodies, body)
	}
	body.space = space
	return body
}

func (space *Space) AddConstraint(constraint *Constraint) *Constraint {
	assert(constraint.space != space, "Already added to this space")
	assert(constraint.space == nil, "Already added to another space")
	assert(space.locked == 0, "Space is locked")

	a := constraint.a
	b := constraint.b
	assert(a != nil && b != nil, "Constraint is attached to a null body")

	a.Activate()
	b.Activate()
	space.constraints = append(space.constraints, constraint)

	// Push onto the heads of the bodies' constraint lists
	constraint.next_a = a.constraintList
	a.constraintList = constraint
	constraint.next_b = b.constraintList
	b.constraintList = constraint
	constraint.space = space

	return constraint
}

func (space *Space) RemoveConstraint(constraint *Constraint) {
	assert(space.ContainsConstraint(constraint), "Constraint not found")
	assert(space.locked == 0, "Space is locked")

	constraint.a.Activate()
	constraint.b.Activate()
	for i, c := range space.constraints {
		if c == constraint {
			space.constraints = append(space.constraints[:i], space.constraints[i+1:]...)
			break
		}
	}

	constraint.a.RemoveConstraint(constraint)
	constraint.b.RemoveConstraint(constraint)
	constraint.space = nil
}

func (space *Space) ContainsConstraint(constraint *Constraint) bool {
	return constraint.space == space
}

var ShapeUpdateFunc = func(shape *Shape, _ interface{}) {
	shape.CacheBB()
}

func SpaceArbiterSetTrans(shapes []*Shape, space *Space) *Arbiter {
	var arb *Arbiter

	select {
	case arb = <-space.pooledArbiters:

	default:
		arb = &Arbiter{}
	}
	arb.Init(shapes[0], shapes[1])
	return arb
}

func SpaceCollideShapesFunc(obj interface{}, b *Shape, collisionId uint32, vspace interface{}) uint32 {
	a := obj.(*Shape)
	space := vspace.(*Space)

	// Reject any of the simple cases
	if QueryReject(a, b) {
		return collisionId
	}

	// Narrow-phase collision detection.
	info := Collide(a, b, collisionId, space.ContactBufferGetArray())

	if info.count == 0 {
		// shapes are not colliding
		return info.collisionId
	}

	//  Push contacts
	space.PushContacts(info.count)

	// Get an arbiter from space->arbiterSet for the two shapes.
	// This is where the persistent contact magic comes from.
	shapePair := []*Shape{info.a, info.b}
	arbHashId := HashPair(HashValue(unsafe.Pointer(info.a)), HashValue(unsafe.Pointer(info.b)))
	arb := space.cachedArbiters.Insert(arbHashId, shapePair, SpaceArbiterSetTrans, space)
	arb.Update(info, space)

	assert(arb.body_a != arb.body_b, "EQUAL")

	if arb.state == CP_ARBITER_STATE_FIRST_COLLISION && !arb.handler.beginFunc(arb, space, arb.handler.userData) {
		arb.Ignore()
	}

	// Ignore the arbiter if it has been flagged
	if arb.state != CP_ARBITER_STATE_IGNORE &&
		// Call preSolve
		arb.handler.preSolveFunc(arb, space, arb.handler.userData) &&
		// Check (again) in case the pre-solve() callback called cpArbiterIgnored().
		arb.state != CP_ARBITER_STATE_IGNORE &&
		// Process, but don't add collisions for sensors.
		!(a.sensor || b.sensor) &&
		// Don't process collisions between two infinite mass bodies.
		// This includes collisions between two kinematic bodies, or a kinematic body and a static body.
		!(a.body.m == INFINITY && b.body.m == INFINITY) {
		space.arbiters = append(space.arbiters, arb)
	} else {
		space.PopContacts(info.count)
		arb.contacts = nil
		arb.count = 0

		// Normally arbiters are set as used after calling the post-solve callback.
		// However, post-solve() callbacks are not called for sensors or arbiters rejected from pre-solve.
		if arb.state != CP_ARBITER_STATE_IGNORE {
			arb.state = CP_ARBITER_STATE_NORMAL
		}
	}

	// Time stamp the arbiter so we know it was used recently.
	arb.stamp = space.stamp
	return info.collisionId
}

func (space *Space) PushFreshContactBuffer() {
	stamp := space.stamp
	head := space.contactBuffersHead

	if head == nil {
		space.contactBuffersHead = NewContactBuffer(stamp, nil)
	} else if stamp-head.next.stamp > space.collisionPersistence {
		tail := head.next
		space.contactBuffersHead = tail.InitHeader(stamp, tail)
	} else {
		// Allocate a new buffer and push it into the ring
		buffer := NewContactBuffer(stamp, head)
		head.next = buffer
		space.contactBuffersHead = buffer
	}
}

func (space *Space) ContactBufferGetArray() []*Contact {
	if space.contactBuffersHead.numContacts+MAX_CONTACTS_PER_ARBITER > CONTACTS_BUFFER_SIZE {
		space.PushFreshContactBuffer()
	}

	head := space.contactBuffersHead
	return head.contacts[head.numContacts:]
}

func QueryReject(a, b *Shape) bool {
	if a.body == b.body {
		return true
	}
	if a.Filter.Reject(b.Filter) {
		return true
	}
	if !a.bb.Intersects(b.bb) {
		return true
	}
	if QueryRejectConstraints(a.body, b.body) {
		return true
	}
	return false
}

func QueryRejectConstraints(a, b *Body) bool {
	for constraint := a.constraintList; constraint != nil; constraint = constraint.Next(a) {
		if !constraint.collideBodies && ((constraint.a == a && constraint.b == b) ||
			(constraint.a == b && constraint.b == a)) {
			return true
		}
	}

	return false
}

func (space *Space) ProcessComponents(dt float64) {
	sleep := space.SleepTimeThreshold != INFINITY

	for _, body := range space.dynamicBodies {
		assert(body.sleepingNext == nil, "Dangling pointer in contact graph (next)")
		assert(body.sleepingRoot == nil, "Dangling pointer in contact graph (root)")
	}

	// calculate the kinetic energy of all the bodies
	if sleep {
		dv := space.idleSpeedThreshold
		var dvsq float64
		if dv != 0 {
			dvsq = dv * dv
		} else {
			dvsq = space.gravity.LengthSq() * dt * dt
		}

		// update idling and reset component nodes
		for _, body := range space.dynamicBodies {
			if body.GetType() != BODY_DYNAMIC {
				continue
			}

			// Need to deal with infinite mass objects
			var keThreshold float64
			if dvsq != 0 {
				keThreshold = body.m * dvsq
			}
			if body.KineticEnergy() > keThreshold {
				body.sleepingIdleTime = 0
			} else {
				body.sleepingIdleTime += dt
			}
		}
	}

	// Awaken any sleeping bodies found and then push arbiters to the bodies' lists.
	for _, arb := range space.arbiters {
		a := arb.body_a
		b := arb.body_b

		if sleep {
			if b.GetType() == BODY_KINEMATIC || a.IsSleeping() {
				a.Activate()
			}
			if a.GetType() == BODY_KINEMATIC || b.IsSleeping() {
				b.Activate()
			}
		}

		a.PushArbiter(arb)
		b.PushArbiter(arb)
	}

	if sleep {
		// Bodies should be held active if connected by a joint to a kinematic.
		for _, constraint := range space.constraints {
			if constraint.b.GetType() == BODY_KINEMATIC {
				constraint.a.Activate()
			}
			if constraint.a.GetType() == BODY_KINEMATIC {
				constraint.b.Activate()
			}
		}

		for i := 0; i < len(space.dynamicBodies); {
			body := space.dynamicBodies[i]

			if body.ComponentRoot() == nil {
				// Body not in a component yet. Perform a DFS to flood fill mark
				// the component in the contact graph using this body as the root.
				FloodFillComponent(body, body)

				// Check if the component should be put to sleep.
				if !ComponentActive(body, space.SleepTimeThreshold) {
					space.sleepingComponents = append(space.sleepingComponents, body)
					for item := body; item != nil; item = item.sleepingNext {
						space.Deactivate(item)
					}

					// Deactivate() removed the current body from the list.
					// Skip incrementing the index counter.
					continue
				}
			}

			i++

			// Only sleeping bodies retain their component node pointers.
			body.sleepingRoot = nil
			body.sleepingNext = nil
		}
	}
}

func ComponentActive(root *Body, threshold float64) bool {
	for item := root; item != nil; item = item.sleepingNext {
		if item.sleepingIdleTime < threshold {
			return true
		}
	}
	return false
}

func FloodFillComponent(root *Body, body *Body) {
	// Kinematic bodies cannot be put to sleep and prevent bodies they are touching from sleeping.
	// Static bodies are effectively sleeping all the time.
	if body.GetType() != BODY_DYNAMIC {
		return
	}

	// body.sleeping.root
	other_root := body.ComponentRoot()
	if other_root == nil {
		// body.sleeping.root = root
		root.ComponentAdd(body)

		for arb := body.arbiterList; arb != nil; arb = ArbiterNext(arb, body) {
			if body == arb.body_a {
				FloodFillComponent(root, arb.body_b)
			} else {
				FloodFillComponent(root, arb.body_a)
			}
		}

		for constraint := body.constraintList; constraint != nil; constraint = constraint.Next(body) {
			if body == constraint.a {
				FloodFillComponent(root, constraint.b)
			} else {
				FloodFillComponent(root, constraint.a)
			}
		}
	} else {
		assert(other_root == root, "Inconsistency detected in the contact graph (FFC)")
	}
}

func ArbiterNext(arb *Arbiter, body *Body) *Arbiter {
	if arb.body_a == body {
		return arb.thread_a.next
	}
	return arb.thread_b.next
}

func (space *Space) Step(dt float64) {
	if dt == 0 {
		return
	}

	space.stamp++

	prev_dt := space.curr_dt
	space.curr_dt = dt

	// reset and empty the arbiter lists
	for _, arb := range space.arbiters {
		arb.state = CP_ARBITER_STATE_NORMAL

		// If both bodies are awake, unthread the arbiter from the contact graph.
		if !arb.body_a.IsSleeping() && !arb.body_b.IsSleeping() {
			arb.Unthread()
		}
	}
	space.arbiters = space.arbiters[:0]

	space.Lock()
	{
		// Integrate positions
		for _, body := range space.dynamicBodies {
			body.position_func(body, dt)
		}

		// Find colliding pairs.
		space.PushFreshContactBuffer()
		space.dynamicShapes.class.Each(ShapeUpdateFunc, nil)
		space.dynamicShapes.class.ReindexQuery(SpaceCollideShapesFunc, space)
	}
	space.Unlock(false)

	// Rebuild the contact graph (and detect sleeping components if sleeping is enabled)
	space.ProcessComponents(dt)

	space.Lock()
	{
		// Clear out old cached arbiters and call separate callbacks
		space.cachedArbiters.Filter(space)

		// Prestep the arbiters and constraints.
		slop := space.collisionSlop
		biasCoef := 1 - math.Pow(space.collisionBias, dt)
		for _, arbiter := range space.arbiters {
			arbiter.PreStep(dt, slop, biasCoef)
		}

		for _, constraint := range space.constraints {
			if constraint.preSolve != nil {
				constraint.preSolve(constraint, space)
			}

			constraint.Class.PreStep(constraint, dt)
		}

		// Integrate velocities.
		damping := math.Pow(space.damping, dt)
		gravity := space.gravity
		for _, body := range space.dynamicBodies {
			body.velocity_func(body, gravity, damping, dt)
		}

		// Apply cached impulses
		var dt_coef float64
		if prev_dt != 0 {
			dt_coef = dt / prev_dt
		}

		for _, arbiter := range space.arbiters {
			arbiter.ApplyCachedImpulse(dt_coef)
		}

		for _, constraint := range space.constraints {
			constraint.Class.ApplyCachedImpulse(constraint, dt_coef)
		}

		// Run the impulse solver.
		var i uint
		for i = 0; i < space.Iterations; i++ {
			for _, arbiter := range space.arbiters {
				arbiter.ApplyImpulse()
			}

			for _, constraint := range space.constraints {
				constraint.Class.ApplyImpulse(constraint, dt)
			}
		}

		// Run the constraint post-solve callbacks
		for _, constraint := range space.constraints {
			if constraint.postSolve != nil {
				constraint.postSolve(constraint, space)
			}
		}

		// run the post-solve callbacks
		for _, arb := range space.arbiters {
			arb.handler.postSolveFunc(arb, space, arb.handler)
		}
	}
	space.Unlock(true)
}

func (space *Space) Lock() {
	space.locked++
}

func (space *Space) Unlock(runPostStep bool) {
	space.locked--

	if space.locked < 0 {
		panic("Space lock underflow")
	}

	if space.locked != 0 {
		return
	}

	for i := 0; i < len(space.rousedBodies); i++ {
		space.Activate(space.rousedBodies[i])
		space.rousedBodies[i] = nil
	}
	space.rousedBodies = space.rousedBodies[0:0]
}

func (space *Space) UncacheArbiter(arb *Arbiter) {
	a := arb.a
	b := arb.b
	shapePair := []*Shape{a, b}
	arbHashId := HashPair(HashValue(unsafe.Pointer(a)), HashValue(unsafe.Pointer(b)))
	space.cachedArbiters.Remove(arbHashId, shapePair)
	for i, a := range space.arbiters {
		if a == arb {
			space.arbiters = append(space.arbiters[0:i], space.arbiters[i+1:]...)
			break
		}
	}
}

func (space *Space) PushContacts(count uint) {
	assert(count <= MAX_CONTACTS_PER_ARBITER, "Contact buffer overflow")
	space.contactBuffersHead.numContacts += count
}

func (space *Space) PopContacts(count uint) {
	space.contactBuffersHead.numContacts -= count
}

func (space *Space) LookupHandler(a, b uint, defaultHandler *CollisionHandler) *CollisionHandler {
	types := &CollisionHandler{typeA: a, typeB: b}
	handler := space.collisionHandlers.Find(HashPair(HashValue(a), HashValue(b)), types)
	if handler != nil {
		return handler
	}
	return defaultHandler
}

func (space *Space) NewCollisionHandler(collisionTypeA, collisionTypeB uint) *CollisionHandler {
	hash := HashPair(HashValue(collisionTypeA), HashValue(collisionTypeB))
	handler := &CollisionHandler{collisionTypeA, collisionTypeB, DefaultBegin, DefaultPreSolve, DefaultPostSolve, DefaultSeparate, nil}
	return space.collisionHandlers.Insert(hash, handler)
}

func (space *Space) NewWildcardCollisionHandler(collisionType uint) *CollisionHandler {
	space.UseWildcardDefaultHandler()

	hash := HashPair(HashValue(collisionType), HashValue(WILDCARD_COLLISION_TYPE))
	handler := &CollisionHandler{collisionType, WILDCARD_COLLISION_TYPE, AlwaysCollide, AlwaysCollide, DoNothing, DoNothing, nil}
	return space.collisionHandlers.Insert(hash, handler)
}

func (space *Space) UseWildcardDefaultHandler() {
	if !space.usesWildcards {
		space.usesWildcards = true
		space.defaultHandler = &CollisionHandlerDefault
	}
}

func (space *Space) UseSpatialHash(dim float64, count int) {
	staticShapes := NewSpaceHash(dim, count, ShapeGetBB, nil)
	dynamicShapes := NewSpaceHash(dim, count, ShapeGetBB, staticShapes)

	space.staticShapes.class.Each(func(shape *Shape, _ interface{}) {
		staticShapes.class.Insert(shape, shape.hashid)
	}, nil)
	space.dynamicShapes.class.Each(func(shape *Shape, _ interface{}) {
		dynamicShapes.class.Insert(shape, shape.hashid)
	}, nil)

	space.staticShapes = staticShapes
	space.dynamicShapes = dynamicShapes
}

func (space *Space) EachBody(f func(body *Body)) {
	space.Lock()
	defer space.Unlock(true)

	for _, body := range space.dynamicBodies {
		f(body)
	}

	for _, body := range space.staticBodies {
		f(body)
	}

	for _, root := range space.sleepingComponents {
		body := root

		for body != nil {
			next := body.sleepingNext
			f(body)
			body = next
		}
	}
}

type SpacePointQueryFunc func(*Shape, Vector, float64, Vector, interface{})

type PointQueryContext struct {
	point       Vector
	maxDistance float64
	filter      ShapeFilter
	f           SpacePointQueryFunc
}

func (space *Space) PointQueryNearest(point Vector, maxDistance float64, filter ShapeFilter) *PointQueryInfo {
	info := &PointQueryInfo{nil, VectorZero(), maxDistance, VectorZero()}
	context := &PointQueryContext{point, maxDistance, filter, nil}

	bb := NewBBForCircle(point, math.Max(maxDistance, 0))
	space.dynamicShapes.class.Query(context, bb, NearestPointQueryNearest, info)
	space.staticShapes.class.Query(context, bb, NearestPointQueryNearest, info)

	return info
}

func NearestPointQueryNearest(obj interface{}, shape *Shape, collisionId uint32, out interface{}) uint32 {
	context := obj.(*PointQueryContext)
	if !shape.Filter.Reject(context.filter) && !shape.sensor {
		info := shape.PointQuery(context.point)
		if info.Distance < out.(*PointQueryInfo).Distance {
			outp := out.(*PointQueryInfo)
			*outp = info
		}
	}

	return collisionId
}

func (space *Space) ArrayForBodyType(bodyType int) *[]*Body {
	if bodyType == BODY_STATIC {
		return &space.staticBodies
	}
	return &space.dynamicBodies
}

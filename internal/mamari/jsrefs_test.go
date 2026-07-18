package mamari

import (
	"strings"
	"testing"
)

// The tests in this file each reproduce one concrete false positive or false
// edge found during accuracy auditing. The fixture shapes cover route-handler registration, scheduled
// job-config objects, addEventListener callbacks, composable returns,
// functions passed as arguments, Vue structural directives, `new X()`
// constructor reachability, `this.member.method()` collisions, import-bound
// receivers with external-lib instances, and duplicate route declarations.

// --- Fix 1: value-position identifier references ------------------------

func TestDeadCodeRouteHandlerPassedToRouterIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "controllers/userController.js", `async function stopShareApplicationsHandler(req, res) {
  res.send({ ok: true })
}
module.exports = { stopShareApplicationsHandler }
`)
	write(t, root, "routes/userRoutes.js", `const { stopShareApplicationsHandler } = require('../controllers/userController')
const express = require('express')
const router = express.Router()
router.post('/applications/stop-share', stopShareApplicationsHandler)
module.exports = router
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "stopShareApplicationsHandler") {
		t.Fatalf("handler registered via router.post must not be dead; edges=%#v", refEdgesFor(idx, "stopShareApplicationsHandler"))
	}
}

func TestDeadCodeJobConfigFunctionIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "background_jobs/CAISExportJob.js", `async function CAISExportJob() {
  return 'exported'
}
module.exports = CAISExportJob
`)
	write(t, root, "background_jobs/jobConfigs.js", `const CAISExportJob = require('./CAISExportJob')
const configs = [
  {
    jobName: 'CAISExportJob',
    schedule: '0 3 * * *',
    jobFunction: CAISExportJob,
  },
]
module.exports = configs
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "CAISExportJob") {
		t.Fatalf("job function registered via jobFunction: config must not be dead; edges=%#v", refEdgesFor(idx, "CAISExportJob"))
	}
}

func TestDeadCodeEventListenerCallbackIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "visibility.js", `function handleWitnessVisibilityChange() {
  return document.hidden
}
function setup() {
  document.addEventListener('visibilitychange', handleWitnessVisibilityChange)
}
setup()
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "handleWitnessVisibilityChange") {
		t.Fatalf("addEventListener callback must not be dead; edges=%#v", refEdgesFor(idx, "handleWitnessVisibilityChange"))
	}
}

func TestDeadCodeFunctionPassedAsArgumentIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "report.js", `function ccdsMinBalanceValue(rows) {
  return Math.min(...rows)
}
function buildReport(rows, valueFn) {
  return valueFn(rows)
}
function run(rows) {
  return buildReport(rows, ccdsMinBalanceValue)
}
run([1])
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "ccdsMinBalanceValue") {
		t.Fatalf("function passed as call argument must not be dead; edges=%#v", refEdgesFor(idx, "ccdsMinBalanceValue"))
	}
}

func TestDeadCodeComposableReturnedMethodIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "useAddressAutocomplete.js", `function handleInput(value) {
  return value
}
export function useAddressAutocomplete() {
  return { handleInput }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "handleInput") {
		t.Fatalf("composable-returned method must not be dead; edges=%#v", refEdgesFor(idx, "handleInput"))
	}
}

// Export sites must NOT count as uses: a CommonJS-exported function that no
// other file imports (and nothing else references) is still dead. This is
// the precision guard that keeps the recall fixes above from wiping out
// dead-code detection entirely.
func TestDeadCodeExportSiteAloneDoesNotMarkAlive(t *testing.T) {
	root := t.TempDir()
	write(t, root, "controller.js", `function pingHandler(req, res) {
  res.send('pong')
}
function usedHandler(req, res) {
  res.send('used')
}
module.exports = { pingHandler, usedHandler }
`)
	write(t, root, "routes.js", `const { usedHandler } = require('./controller')
const express = require('express')
const router = express.Router()
router.get('/used', usedHandler)
module.exports = router
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if !isDeadCodeCandidate(resp, "pingHandler") {
		t.Fatalf("exported-but-never-imported pingHandler must STILL be dead (export sites are not uses); edges=%#v", refEdgesFor(idx, "pingHandler"))
	}
	if isDeadCodeCandidate(resp, "usedHandler") {
		t.Fatalf("usedHandler is registered on a route and must not be dead")
	}
}

// A recursive self-registration (setTimeout(loop, ...)) inside the function's
// own body is not external evidence of use.
func TestDeadCodeSelfReferenceDoesNotMarkAlive(t *testing.T) {
	root := t.TempDir()
	write(t, root, "loop.js", `function orphanLoop() {
  setTimeout(orphanLoop, 1000)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if !isDeadCodeCandidate(resp, "orphanLoop") {
		t.Fatalf("self-referencing orphan must still be dead; edges=%#v", refEdgesFor(idx, "orphanLoop"))
	}
}

// Renamed destructured require: `const { a: b } = require('./x')` — a use of
// the local alias must resolve to the *imported* declaration.
func TestValueRefResolvesRenamedImportBinding(t *testing.T) {
	root := t.TempDir()
	write(t, root, "jobs.js", `function realJob() {
  return 1
}
module.exports = { realJob }
`)
	write(t, root, "scheduler.js", `const { realJob: aliasedJob } = require('./jobs')
const table = [{ run: aliasedJob }]
module.exports = table
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "realJob") {
		t.Fatalf("renamed import binding use must keep realJob alive; edges=%#v", refEdgesFor(idx, "realJob"))
	}
}

// --- Fix 1b: Vue structural directives + template identifier refs -------

func TestVueVIfCallKeepsScriptMethodAlive(t *testing.T) {
	root := t.TempDir()
	write(t, root, "plaidCard.vue", `<template>
  <div v-if="isNearExpiry(account.expiry)">expiring</div>
</template>
<script setup>
function isNearExpiry(date) {
  return new Date(date) < new Date()
}
const account = { expiry: '2026-01-01' }
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "isNearExpiry") {
		t.Fatalf("method called from v-if must not be dead; edges=%#v", refEdgesFor(idx, "isNearExpiry"))
	}
}

// A call buried inside a *mixed* structural-directive expression (not a bare
// handler name) must still produce a calls edge, and identifiers selecting
// between handlers (ternary) must produce reference edges — otherwise the
// non-taken branch's handler looks dead. (Primitive consts like
// `const canEdit = true` emit no symbol, so there is deliberately no
// assertion for them: nothing exists to reference.)
func TestVueMixedExpressionCallsAndTernaryHandlerRefs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "gate.vue", `<template>
  <button v-show="canEdit && isUnlocked(row)">edit</button>
  <FormInput :validator="strict ? validateStrict : validateLoose" />
</template>
<script setup>
const canEdit = true
const strict = true
function isUnlocked(row) {
  return !row.locked
}
function validateStrict(v) {
  return v.length > 3
}
function validateLoose(v) {
  return v.length > 0
}
const row = {}
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	foundCall, refStrict, refLoose := false, false, false
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.Contains(e.To, "isUnlocked") {
			foundCall = true
		}
		if e.Type == "references-symbol" && strings.Contains(e.To, "validateStrict") {
			refStrict = true
		}
		if e.Type == "references-symbol" && strings.Contains(e.To, "validateLoose") {
			refLoose = true
		}
	}
	if !foundCall {
		t.Fatalf("expected calls edge to isUnlocked from v-show expression")
	}
	if !refStrict || !refLoose {
		t.Fatalf("expected references-symbol edges to both ternary handler branches (strict=%v loose=%v)", refStrict, refLoose)
	}
}

// --- Fix 2: new ClassName() reaches the constructor method --------------

func TestNewExpressionEmitsConstructorCallEdge(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Service.js", `class ReconService {
  constructor(deps) {
    this.deps = deps
  }
  run() {
    return this.deps
  }
}
module.exports = ReconService
`)
	write(t, root, "Service.test.js", `const ReconService = require('./Service')
test('constructs', () => {
  const svc = new ReconService({})
  svc.run()
})
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	classEdge, ctorEdge := false, false
	for _, e := range snap.SymbolEdges {
		if e.Type != "calls" || e.Confidence == ConfUnresolved {
			continue
		}
		if strings.HasSuffix(e.To, ":ReconService") {
			classEdge = true
		}
		if strings.HasSuffix(e.To, "ReconService.constructor") {
			ctorEdge = true
		}
	}
	if !classEdge {
		t.Fatalf("existing class-targeted new-edge must be preserved")
	}
	if !ctorEdge {
		t.Fatalf("new ReconService() must also produce a calls edge to the constructor method; edges=%#v", snap.SymbolEdges)
	}
}

// --- Fix 3: this.member.method() must not match same-class names --------

func TestThisMemberMethodDoesNotMatchSameClassName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "MLService.js", `class MLService {
  categorizeTransactions(rows) {
    return rows
  }
}
module.exports = MLService
`)
	write(t, root, "Pipeline.js", `const MLService = require('./MLService')
class Pipeline {
  constructor() {
    this.mlService = new MLService()
  }
  categorizeTransactions(rows) {
    return this._advanced(rows)
  }
  _advanced(rows) {
    return this.mlService.categorizeTransactions(rows)
  }
}
module.exports = Pipeline
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type != "calls" || e.Evidence.Raw != "this.mlService.categorizeTransactions" {
			continue
		}
		if strings.Contains(e.To, "Pipeline.categorizeTransactions") {
			t.Fatalf("this.mlService.categorizeTransactions must NOT resolve to the same-class method: %#v", e)
		}
		if strings.Contains(e.To, "MLService.categorizeTransactions") {
			return // resolved to the member's class — correct
		}
	}
	// Falling through means the call resolved to neither; unresolved is
	// acceptable (honest), same-class is the only failure — assert we at
	// least saw the call site.
	found := false
	for _, e := range snap.SymbolEdges {
		if e.Evidence.Raw == "this.mlService.categorizeTransactions" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a recorded call for this.mlService.categorizeTransactions")
	}
}

// Plain single-hop this-calls must keep resolving to the enclosing class.
func TestThisSingleHopStillResolvesScoped(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Runner.js", `class Runner {
  start() {
    return this.step()
  }
  step() {
    return 1
  }
}
module.exports = Runner
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, "Runner.step") && e.Confidence == ConfScoped {
			return
		}
	}
	t.Fatalf("this.step() must still resolve scoped to Runner.step; edges=%#v", snap.SymbolEdges)
}

// --- Fix 3b: import-bound receiver with no such method → unresolved -----

func TestImportBoundReceiverMissingMethodIsUnresolvedNotGuessed(t *testing.T) {
	root := t.TempDir()
	// loggingService exports a pino-like instance: no `info` symbol exists.
	write(t, root, "loggingService.js", `const pino = require('pino')
const logger = pino()
module.exports = logger
`)
	// An unrelated class also defines info() — the old fall-through matched it.
	write(t, root, "CronJobLogger.js", `class CronJobLogger {
  info(msg) {
    return msg
  }
}
module.exports = CronJobLogger
`)
	write(t, root, "service.js", `const logger = require('./loggingService')
function work() {
  logger.info('processing')
}
module.exports = { work }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type != "calls" || e.Evidence.Raw != "logger.info" || e.Evidence.File != "service.js" {
			continue
		}
		if strings.Contains(e.To, "CronJobLogger") {
			t.Fatalf("logger.info on an import-bound receiver must not match unrelated CronJobLogger.info: %#v", e)
		}
		if e.Confidence != ConfUnresolved {
			t.Fatalf("expected unresolved for import-bound receiver with missing method, got %#v", e)
		}
		return
	}
	t.Fatalf("expected a recorded call edge for logger.info in service.js")
}

// --- Fix 3b: vue-emit metadata is not a call target ----------------------

func TestMemberCallDoesNotResolveToVueEmitSymbol(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Details.vue", `<template>
  <div>details</div>
</template>
<script setup>
import { ElLoading } from 'element-plus'
const emit = defineEmits(['close', 'view-company'])
function displayAppl() {
  const loading = ElLoading.service({ text: 'loading' })
  loading.close()
}
displayAppl()
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type != "calls" || e.Evidence.Raw != "loading.close" {
			continue
		}
		if target, ok := snap.Symbols[e.To]; ok && target.Kind == "vue-emit" {
			t.Fatalf("loading.close() must not resolve to the defineEmits 'close' entry: %#v", e)
		}
	}
}

// --- Fix 4: duplicate exact route declarations ---------------------------

func TestHTTPRouteExactDuplicateEmitsAllCandidatesAsHeuristic(t *testing.T) {
	root := t.TempDir()
	// Alphabetically-first stale copy — the old behavior linked only here.
	write(t, root, "backend/class_js_backup.js", `const express = require('express')
const app = express()
app.post('/displayLoan', function oldHandler(req, res) {
  res.send('stale')
})
`)
	write(t, root, "backend/routes/legacyRoutes.js", `const express = require('express')
const router = express.Router()
router.post('/displayLoan', function liveHandler(req, res) {
  res.send('live')
})
module.exports = router
`)
	write(t, root, "frontend/loans.js", `const apiClient = require('./apiClient')
async function displayLoan(id) {
  return apiClient.post('/displayLoan', { id })
}
module.exports = { displayLoan }
`)
	write(t, root, "frontend/apiClient.js", `module.exports = { post: async () => ({}) }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	var targets []string
	for _, e := range snap.SymbolEdges {
		if e.Type != "calls-http-route" || e.Evidence.File != "frontend/loans.js" {
			continue
		}
		if e.Confidence != ConfHeuristic {
			t.Fatalf("exact-duplicate route match must be heuristic, got %#v", e)
		}
		if sym, ok := snap.Symbols[e.To]; ok {
			targets = append(targets, sym.File)
		}
	}
	if len(targets) != 2 {
		t.Fatalf("expected calls-http-route edges to BOTH duplicate declarations, got files=%v", targets)
	}
}

func TestHTTPRouteUniqueMatchStaysExact(t *testing.T) {
	root := t.TempDir()
	write(t, root, "backend/routes.js", `const express = require('express')
const router = express.Router()
router.post('/uniquePath', function handler(req, res) {
  res.send('ok')
})
module.exports = router
`)
	write(t, root, "frontend/client.js", `const apiClient = require('./apiClient')
async function callIt() {
  return apiClient.post('/uniquePath', {})
}
module.exports = { callIt }
`)
	write(t, root, "frontend/apiClient.js", `module.exports = { post: async () => ({}) }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls-http-route" && e.Evidence.File == "frontend/client.js" {
			if e.Confidence != ConfExact {
				t.Fatalf("unique exact route match must stay exact, got %#v", e)
			}
			return
		}
	}
	t.Fatalf("expected a calls-http-route edge from frontend/client.js")
}

// --- helpers -------------------------------------------------------------

// refEdgesFor lists edges into any symbol with the given name — the debugging
// payload for the assertions above.
func refEdgesFor(idx *Index, name string) []CGPEdge {
	snap := idx.snapshot()
	var ids []string
	for id, sym := range snap.Symbols {
		if sym.Name == name {
			ids = append(ids, id)
		}
	}
	var out []CGPEdge
	for _, e := range snap.SymbolEdges {
		for _, id := range ids {
			if e.To == id {
				out = append(out, e)
			}
		}
	}
	return out
}

// A concise-arrow helper (`const build = () => new Service({...})`) carries
// the same return-type evidence as a block body with `return new Service()`.
// Locals built through it must resolve their method calls into the service's
// file — the last untested-flag miss from the 2026-07-02 audit.
func TestConciseArrowHelperReturnTypeResolvesMethods(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Service.js", `class ReportService {
  constructor(deps) {
    this.deps = deps
  }
  buildTransactionsReport(args) {
    return args
  }
}
module.exports = ReportService
`)
	write(t, root, "Service.test.js", `const ReportService = require('./Service')
const buildService = (overrides = {}) => new ReportService({ ...overrides })
test('builds a report', async () => {
  const service = buildService()
  await service.buildTransactionsReport({ applicationId: 1 })
})
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, "ReportService.buildTransactionsReport") && e.Confidence != ConfUnresolved {
			return
		}
	}
	t.Fatalf("service.buildTransactionsReport() via concise-arrow helper must resolve; edges=%#v", snap.SymbolEdges)
}

// The real-repo shape: the concise-arrow helper lives inside a describe()
// callback and is called from sibling test() callbacks — different scopes,
// connected only by the bare helper name (unique in the file).
func TestConciseArrowHelperInsideDescribeResolvesMethods(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Service.js", `class ReportService {
  constructor(deps) {
    this.deps = deps
  }
  buildTransactionsReport(args) {
    return args
  }
}
module.exports = ReportService
`)
	write(t, root, "Service.test.js", `const ReportService = require('./Service')
describe('ReportService', () => {
  const buildService = (overrides = {}) => new ReportService({ ...overrides })

  test('builds a report', async () => {
    const service = buildService()
    await service.buildTransactionsReport({ applicationId: 1 })
  })
})
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, "ReportService.buildTransactionsReport") && e.Confidence != ConfUnresolved {
			return
		}
	}
	t.Fatalf("service.buildTransactionsReport() via describe-scoped concise-arrow helper must resolve; edges=%#v", snap.SymbolEdges)
}

// --- Fix 5 (B): top-level entry-point call attributed to the file ------

func TestDeadCodeEntryPointSelfCallIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "migrate.js", `const db = require('./db')
async function runMigration() {
  return db.connect()
}
runMigration();
`)
	write(t, root, "db.js", `module.exports = { connect: async () => ({}) }`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "runMigration") {
		t.Fatalf("top-level entry-point call runMigration() must mark it used; edges=%#v", refEdgesFor(idx, "runMigration"))
	}
}

// --- Fix 6 (C): renamed default-export registration -------------------

func TestDeadCodeRenamedDefaultExportJobIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "aggregateLoansInfo_job.js", `async function aggregateLoansInfoJob() {
  return 1
}
function helperNotExported() { return 2 }
module.exports = aggregateLoansInfoJob
`)
	write(t, root, "jobConfigs.js", `const aggregateLoansInfo = require('./aggregateLoansInfo_job')
const configs = [
  { jobName: 'aggregateLoansInfo', jobFunction: aggregateLoansInfo },
]
module.exports = configs
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "aggregateLoansInfoJob") {
		t.Fatalf("renamed default-export job registered via jobFunction must not be dead; edges=%#v", refEdgesFor(idx, "aggregateLoansInfoJob"))
	}
	// helperNotExported is genuinely unused → must still be dead (guard that
	// the default-export fallback didn't blanket-clear the whole file).
	if !isDeadCodeCandidate(resp, "helperNotExported") {
		t.Fatalf("helperNotExported has no reference and must still be dead")
	}
}

// --- Fix 7 (A): lazy-loaded Vue Router route component ----------------

func TestDeadCodeLazyRouteComponentIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "components/PartnersView.vue", `<template><div>partners</div></template>
<script setup>
const title = 'Partners'
</script>
`)
	write(t, root, "router/index.js", `const routes = [
  {
    path: '/partners',
    name: 'partners',
    component: () => import('@/components/PartnersView.vue'),
  },
]
export default routes
`)
	// '@/' resolves from repo root in importFileCandidates, so PartnersView
	// must sit at components/PartnersView.vue relative to root.
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "PartnersView") {
		t.Fatalf("lazy-loaded route component must not be dead; edges=%#v", refEdgesFor(idx, "PartnersView"))
	}
}

// A relative-path lazy import must also resolve.
func TestDynamicImportRelativePathEmitsEdge(t *testing.T) {
	root := t.TempDir()
	write(t, root, "views/Dash.vue", `<template><div>d</div></template><script setup>const x=1</script>`)
	write(t, root, "router.js", `const routes = [{ component: () => import('./views/Dash.vue') }]
export default routes
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "references-symbol" && e.Evidence.Kind == "dynamic-import" && strings.Contains(e.To, "Dash") {
			return
		}
	}
	t.Fatalf("dynamic import('./views/Dash.vue') must emit a references-symbol edge to the component; edges=%#v", snap.SymbolEdges)
}

// In a monorepo the `@` alias maps to each app's own src dir, not the repo
// root. A route component lazy-imported via `@/components/X.vue` from
// frontend/src must resolve to frontend/src/components/X.vue.
func TestDeadCodeMonorepoAliasLazyRouteComponent(t *testing.T) {
	root := t.TempDir()
	write(t, root, "frontend/src/components/Partners/PartnersView.vue", `<template><div>partners</div></template>
<script setup>
const title = 'Partners'
</script>
`)
	write(t, root, "frontend/src/router/index.js", `const routes = [
  { path: '/partners', component: () => import('@/components/Partners/PartnersView.vue') },
]
export default routes
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "PartnersView") {
		t.Fatalf("monorepo @/-aliased lazy route component must not be dead; edges=%#v", refEdgesFor(idx, "PartnersView"))
	}
}

// A static `@/` import in a monorepo app must resolve cross-file too (this is
// the broad win — every frontend @/ import, not just lazy ones).
func TestMonorepoAliasStaticImportResolves(t *testing.T) {
	root := t.TempDir()
	write(t, root, "frontend/src/utils/format.js", `export function formatMoney(n) { return '$' + n }`)
	write(t, root, "frontend/src/components/Bill.vue", `<template><div>{{ formatMoney(1) }}</div></template>
<script setup>
import { formatMoney } from '@/utils/format'
const total = formatMoney(10)
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, "format.js:formatMoney") && e.Confidence != ConfUnresolved {
			return
		}
	}
	t.Fatalf("static @/utils/format import must resolve formatMoney cross-file; edges=%#v", snap.SymbolEdges)
}

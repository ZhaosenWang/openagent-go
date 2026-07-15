import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { PlanDef, StepState, SSEEvent, PendingApproval } from '@/types'
import { connectSSE } from '@/api/sse'
import * as api from '@/api'

// ── Per-session state ──

interface PlanPane {
  planDef: PlanDef | null
  steps: Record<string, StepState>
  executing: boolean
  planError: string | null
  waitingRetry: string | null
  gatePause: boolean
  planDone: boolean
  pendingApproval: PendingApproval | null
  _approvalQueue: PendingApproval[]
  replanning: boolean
  thinkingText: string
  eventCleanup: (() => void) | null
}

export const usePlanStore = defineStore('plan', () => {
  // ── Per-session pane cache ──
  const _panes = new Map<string, PlanPane>()

  function makePane(): PlanPane {
    return {
      planDef: null,
      steps: {},
      executing: false,
      planError: null,
      waitingRetry: null,
      gatePause: false,
      planDone: false,
      pendingApproval: null,
      _approvalQueue: [],
      replanning: false,
      thinkingText: '',
      eventCleanup: null,
    }
  }

  function getPane(sid: string): PlanPane {
    let p = _panes.get(sid)
    if (!p) {
      p = makePane()
      _panes.set(sid, p)
    }
    return p
  }

  // ── Active-session views ──
  const planDef = ref<PlanDef | null>(null)
  const steps = ref<Record<string, StepState>>({})
  const executing = ref(false)
  const planError = ref<string | null>(null)
  const waitingRetry = ref<string | null>(null)
  const gatePause = ref(false)
  const planDone = ref(false)
  const pendingApproval = ref<PendingApproval | null>(null)
  const _approvalQueue: PendingApproval[] = []
  const replanning = ref(false)
  const thinkingText = ref('')

  const currentSessionId = ref<string | null>(null)

  // ── Save / restore ──

  function saveActive() {
    const sid = currentSessionId.value
    if (!sid) return
    const p = getPane(sid)
    p.planDef = planDef.value
    p.steps = steps.value
    p.executing = executing.value
    p.planError = planError.value
    p.waitingRetry = waitingRetry.value
    p.gatePause = gatePause.value
    p.planDone = planDone.value
    p.pendingApproval = pendingApproval.value
    p._approvalQueue = [..._approvalQueue]
    p.replanning = replanning.value
    p.thinkingText = thinkingText.value
    // eventCleanup is the live SSE connection — keep it on the pane
  }

  function restoreActive(sid: string) {
    const p = getPane(sid)
    planDef.value = p.planDef
    steps.value = p.steps
    executing.value = p.executing
    planError.value = p.planError
    waitingRetry.value = p.waitingRetry
    gatePause.value = p.gatePause
    planDone.value = p.planDone
    pendingApproval.value = p.pendingApproval
    _approvalQueue.length = 0; _approvalQueue.push(...p._approvalQueue)
    replanning.value = p.replanning
    thinkingText.value = p.thinkingText
  }

  function activateSession(sid: string) {
    if (currentSessionId.value === sid) return
    saveActive()
    currentSessionId.value = sid
    restoreActive(sid)
  }

  // ── Plan logic ──

  function initSteps(def: PlanDef) {
    const map: Record<string, StepState> = {}
    for (const step of def.steps) {
      map[step.id] = { status: 'pending', output: '', summary: '', toolCalls: [] }
    }
    steps.value = map
  }

  async function generatePlan(sessionId: string, goal: string, onThinking: (text: string) => void) {
    const p = getPane(sessionId)
    p.planError = null
    try {
      const def = await api.generatePlan(sessionId, goal, onThinking)
      p.planDef = def
      p.planDone = false
      // If this is the active session, sync to reactive refs.
      if (sessionId === currentSessionId.value) {
        planDef.value = def
        planDone.value = false
        initSteps(def)
      }
      // Always update the pane's steps.
      const map: Record<string, StepState> = {}
      for (const step of def.steps) {
        map[step.id] = { status: 'pending', output: '', summary: '', toolCalls: [] }
      }
      p.steps = map
      if (sessionId === currentSessionId.value) steps.value = map
    } catch (e: any) {
      p.planError = e.message
      if (sessionId === currentSessionId.value) planError.value = e.message
    }
  }

  async function executePlan(sessionId: string) {
    const p = getPane(sessionId)
    if (!p.planDef) return
    p.planError = null
    p.waitingRetry = null
    p.gatePause = false
    p.planDone = false

    // Subscribe to events BEFORE triggering execution.
    p.eventCleanup?.()
    p.eventCleanup = connectSSE(
      `/plan/sessions/${sessionId}/events`,
      (evt) => handlePlanEvent(p, evt),
      (err) => console.error('plan SSE error:', err),
    )

    try {
      await api.executePlan(sessionId)
      p.executing = true
      if (sessionId === currentSessionId.value) {
        planError.value = null
        waitingRetry.value = null
        gatePause.value = false
        planDone.value = false
        executing.value = true
      }
    } catch (e: any) {
      p.planError = e.message
      p.eventCleanup?.()
      if (sessionId === currentSessionId.value) {
        planError.value = e.message
        eventCleanupCleanup(p)
      }
    }
  }

  function handlePlanEvent(p: PlanPane, event: SSEEvent) {
    switch (event.type) {
      case 'step_start': {
        if (event.step_id && p.steps[event.step_id]) {
          p.steps[event.step_id].status = 'running'
        }
        if (currentSessionId.value && getPane(currentSessionId.value) === p) {
          if (event.step_id && steps.value[event.step_id]) {
            steps.value[event.step_id].status = 'running'
          }
        }
        break
      }
      case 'step_text_delta': {
        if (event.step_id && p.steps[event.step_id]) {
          p.steps[event.step_id].output += event.text || ''
        }
        break
      }
      case 'step_tool_call': {
        if (event.step_id && p.steps[event.step_id]) {
          p.steps[event.step_id].toolCalls.push({
            name: event.tool_call?.function.name || 'unknown',
            args: event.tool_call?.function.arguments || '',
          })
        }
        break
      }
      case 'step_tool_progress': {
        if (event.step_id) {
          const step = p.steps[event.step_id]
          if (step) step.output += event.text || ''
        }
        break
      }
      case 'step_tool_result': {
        if (event.step_id) {
          const step = p.steps[event.step_id]
          if (step) {
            const lastCall = step.toolCalls[step.toolCalls.length - 1]
            if (lastCall) lastCall.result = event.text || ''
            step.output += `\nResult: ${event.text || ''}`
          }
        }
        break
      }
      case 'step_done': {
        if (event.step_id && p.steps[event.step_id]) {
          p.steps[event.step_id].status = 'done'
          p.steps[event.step_id].summary = event.text || ''
        }
        break
      }
      case 'step_failed': {
        if (event.step_id && p.steps[event.step_id]) {
          p.steps[event.step_id].status = 'failed'
          p.steps[event.step_id].error = event.error || ''
        }
        break
      }
      case 'tool_approval': {
        if (event.tool_call) {
          const approval: PendingApproval = { toolCall: event.tool_call, sessionId: '', sessionType: 'plan' }
          p._approvalQueue.push(approval)
          if (!p.pendingApproval) p.pendingApproval = p._approvalQueue[0]
          if (currentSessionId.value && getPane(currentSessionId.value) === p) {
            _approvalQueue.push(approval)
            if (!pendingApproval.value) pendingApproval.value = _approvalQueue[0]
          }
        }
        break
      }
      case 'replanning': {
        p.replanning = true; p.thinkingText = ''
        if (currentSessionId.value && getPane(currentSessionId.value) === p) {
          replanning.value = true; thinkingText.value = ''
        }
        break
      }
      case 'plan_thinking': {
        p.thinkingText += event.text || ''
        break
      }
      case 'plan_waiting_retry': {
        const sid = event.step_id || null
        p.waitingRetry = sid
        p.executing = false
        p.gatePause = sid ? p.steps[sid]?.status === 'done' : false
        if (currentSessionId.value && getPane(currentSessionId.value) === p) {
          waitingRetry.value = sid
          executing.value = false
          gatePause.value = p.gatePause
        }
        break
      }
      case 'plan_generated': {
        p.replanning = false; p.thinkingText = ''
        if (event.text) {
          try {
            const def = JSON.parse(event.text) as PlanDef
            p.planDef = def
            const map: Record<string, StepState> = {}
            for (const step of def.steps) {
              map[step.id] = { status: 'pending', output: '', summary: '', toolCalls: [] }
            }
            p.steps = map
            if (currentSessionId.value && getPane(currentSessionId.value) === p) {
              planDef.value = def
              steps.value = map
            }
          } catch { /* ignore */ }
        }
        if (currentSessionId.value && getPane(currentSessionId.value) === p) {
          replanning.value = false; thinkingText.value = ''
        }
        break
      }
      case 'plan_done': {
        p.replanning = false; p.thinkingText = ''; p.executing = false; p.planDone = true
        p.eventCleanup?.()
        if (currentSessionId.value && getPane(currentSessionId.value) === p) {
          replanning.value = false; thinkingText.value = ''; executing.value = false; planDone.value = true
        }
        break
      }
      case 'plan_error': {
        p.planError = event.error || 'Plan error'; p.executing = false; p.replanning = false
        p.eventCleanup?.()
        if (currentSessionId.value && getPane(currentSessionId.value) === p) {
          planError.value = event.error || 'Plan error'; executing.value = false; replanning.value = false
        }
        break
      }
      case 'plan_cancelled': {
        p.executing = false; p.replanning = false
        p.eventCleanup?.()
        if (currentSessionId.value && getPane(currentSessionId.value) === p) {
          executing.value = false; replanning.value = false
        }
        break
      }
    }
  }

  async function approveTool(sessionId: string, allowed: boolean, feedback?: string) {
    if (!pendingApproval.value) return
    _approvalQueue.shift()
    pendingApproval.value = _approvalQueue[0] || null
    try { await api.approvePlanTool(sessionId, allowed, feedback) }
    catch (e) { console.error('plan approveTool:', e) }
  }

  async function cancelExecution(sessionId: string) {
    try {
      await api.cancelPlan(sessionId)
      executing.value = false
      const p = getPane(sessionId)
      p.executing = false
      eventCleanupCleanup(p)
    } catch (e: any) { console.error('cancelPlan:', e) }
  }

  async function retryStep(sessionId: string, stepId: string) {
    try {
      await api.retryPlanStep(sessionId, stepId)
      waitingRetry.value = null; gatePause.value = false; executing.value = true
      const p = getPane(sessionId)
      p.waitingRetry = null; p.gatePause = false; p.executing = true
    } catch (e: any) { planError.value = e.message }
  }

  async function replan(sessionId: string, feedback: string) {
    try {
      pendingApproval.value = null
      await api.replan(sessionId, feedback)
      waitingRetry.value = null; gatePause.value = false; executing.value = true
      const p = getPane(sessionId)
      p.waitingRetry = null; p.gatePause = false; p.executing = true; p.pendingApproval = null
    } catch (e: any) { planError.value = e.message }
  }

  function eventCleanupCleanup(p: PlanPane) {
    p.eventCleanup?.()
    p.eventCleanup = null
  }

  function clearPlan() {
    const sid = currentSessionId.value
    if (sid) {
      const p = getPane(sid)
      p.eventCleanup?.()
      _panes.delete(sid)
    }
    planDef.value = null; steps.value = {}; executing.value = false
    planError.value = null; waitingRetry.value = null; gatePause.value = false
    planDone.value = false; pendingApproval.value = null; _approvalQueue.length = 0
    replanning.value = false; thinkingText.value = ''
  }

  return {
    planDef, steps, executing, planError, waitingRetry, planDone,
    gatePause,
    pendingApproval, replanning, thinkingText,
    currentSessionId,
    activateSession,
    generatePlan, executePlan, approveTool, cancelExecution, retryStep, replan, clearPlan,
  }
})

<template>
  <n-scrollbar class="plan-view">
    <!-- Empty state: create plan -->
    <n-card v-if="!plan.planDef" title="Create a Plan" class="plan-create">
      <n-text depth="3">Describe your goal and the planner will generate a step-by-step plan.</n-text>
      <n-input
        v-model:value="goal"
        type="textarea"
        :autosize="{ minRows: 2, maxRows: 4 }"
        placeholder="e.g. Write a Go function that reverses a string and includes a test"
        class="goal-input"
        @keydown.enter="handleGenerate"
      />
      <n-button type="primary" :loading="generating" :disabled="!goal.trim()" @click="handleGenerate" block>
        {{ generatingText || 'Generate Plan' }}
      </n-button>
      <div v-if="generating && thinkingOutput" class="thinking-box">
        <n-collapse>
          <n-collapse-item title="Planning...">
            <pre class="thinking-text">{{ thinkingOutput }}</pre>
          </n-collapse-item>
        </n-collapse>
      </div>
      <n-alert v-if="plan.planError" type="error" :title="plan.planError" style="margin-top:12px" />
    </n-card>

    <!-- Plan active -->
    <template v-else>
      <n-space vertical size="medium">
        <!-- Header -->
        <n-card size="small">
          <template #header>
            <div class="plan-header">
              <n-text strong>{{ plan.planDef.goal }}</n-text>
              <n-space>
                <n-button v-if="!plan.executing && !plan.planDone" type="primary" size="small" @click="handleExecute">Execute</n-button>
                <n-button v-if="!plan.executing && !plan.planDone" size="small" @click="showReplan = true">Replan</n-button>
                <n-button v-if="plan.executing" type="error" size="small" @click="handleCancel">Cancel</n-button>
                <n-button size="small" @click="plan.clearPlan()">Clear</n-button>
              </n-space>
            </div>
          </template>
        </n-card>

        <!-- Replanning thinking -->
        <n-card v-if="plan.replanning" size="small" class="replan-alert">
          <n-collapse>
            <n-collapse-item title="Replanning...">
              <pre class="thinking-text">{{ plan.thinkingText || '(thinking...)' }}</pre>
            </n-collapse-item>
          </n-collapse>
        </n-card>

        <!-- Pre-execution replan -->
        <n-card v-if="showReplan" size="small" class="replan-card">
          <n-text depth="2">What would you like to change about this plan?</n-text>
          <n-input
            v-model:value="preExecFeedback"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="e.g. Add a testing step, use researcher instead of architect..."
            style="margin:8px 0"
          />
          <div v-if="replanning && thinkingOutput" class="thinking-box">
            <n-collapse>
              <n-collapse-item title="Regenerating...">
                <pre class="thinking-text">{{ thinkingOutput }}</pre>
              </n-collapse-item>
            </n-collapse>
          </div>
          <n-space>
            <n-button size="small" type="primary" :loading="replanning" :disabled="!preExecFeedback.trim()" @click="handlePreReplan">Regenerate</n-button>
            <n-button size="small" @click="showReplan = false; preExecFeedback = ''">Cancel</n-button>
          </n-space>
        </n-card>

        <!-- DAG -->
        <PlanDAG :steps="plan.planDef.steps" :step-state="stepState" />

        <!-- Gate pause (step done, waiting for approval) -->
        <n-card v-if="plan.waitingRetry && plan.gatePause" class="gate-card">
          <n-text>Step "{{ plan.waitingRetry }}" completed. Review the output and continue.</n-text>
          <n-space style="margin-top:8px">
            <n-button type="primary" size="small" @click="handleRetry(plan.waitingRetry!)">Continue</n-button>
            <n-button size="small" @click="showReplan = true">Replan</n-button>
          </n-space>
          <n-card v-if="showReplan" size="small" class="replan-card" style="margin-top:12px">
            <n-text depth="2">What would you like to change?</n-text>
            <n-input
              v-model:value="feedback"
              type="textarea"
              :autosize="{ minRows: 2, maxRows: 4 }"
              placeholder="e.g. Use HA database instead..."
            />
            <n-space style="margin-top:8px">
              <n-button size="small" type="primary" :disabled="!feedback.trim()" @click="handleReplan">Replan with Feedback</n-button>
              <n-button size="small" @click="showReplan = false; feedback = ''">Cancel</n-button>
            </n-space>
          </n-card>
        </n-card>

        <!-- Failed step pause (retry or replan) -->
        <n-card v-if="plan.waitingRetry && !plan.gatePause" class="retry-card">
          <n-text>Step "{{ plan.waitingRetry }}" failed. What would you like to do?</n-text>
          <n-space style="margin-top:8px">
            <n-button size="small" @click="handleRetry(plan.waitingRetry!)">Retry</n-button>
            <n-input v-model:value="feedback" size="small" placeholder="Optional feedback..." style="width:200px" />
            <n-button size="small" :disabled="!feedback.trim()" @click="handleReplan">Replan</n-button>
          </n-space>
        </n-card>

        <n-alert v-if="plan.planDone" type="success" title="Plan completed." />
        <n-alert v-if="plan.planError" type="error" :title="plan.planError" />
      </n-space>
    </template>
    <!-- Tool approval (during plan execution) -->
    <ToolApprovalDialog
      v-if="plan.pendingApproval"
      :key="plan.pendingApproval.toolCall.id"
      :tool-call="plan.pendingApproval.toolCall"
      @resolve="handleApprove"
    />
  </n-scrollbar>
</template>

<script setup lang="ts">
import { ref, computed, watchEffect, onBeforeUnmount } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { NScrollbar, NCard, NInput, NButton, NSpace, NText, NTag, NCollapse, NCollapseItem, NAlert } from 'naive-ui'
import { usePlanStore } from '@/stores/plan'
import { useSessionsStore } from '@/stores/sessions'
import type { StepState } from '@/types'
import PlanDAG from '@/components/plan/PlanDAG.vue'
import ToolApprovalDialog from '@/components/chat/ToolApprovalDialog.vue'

const route = useRoute()
const router = useRouter()
const plan = usePlanStore()
const sessions = useSessionsStore()

const goal = ref('')
const generating = ref(false)
const generatingText = ref('')
const thinkingOutput = ref('')
const feedback = ref('')
const showReplan = ref(false)
const preExecFeedback = ref('')
const replanning = ref(false)

const sessionId = computed(() => (route.params.sessionId as string) || sessions.currentSessionId || '')

// Activate session on route change — preserves plan state in per-session cache.
watchEffect(() => {
  const sid = sessionId.value
  if (!sid) return
  plan.activateSession(sid)
})

// Session creation is handled by AppSidebar on mode switch.
// PlanView only needs to activate the already-created session.

function stepState(id: string): StepState {
  return plan.steps[id] || { status: 'pending', output: '', summary: '', toolCalls: [] }
}

async function handleGenerate() {
  const g = goal.value.trim()
  if (!g) return
  generating.value = true
  generatingText.value = 'Planning...'
  try {
    let sid = sessionId.value
    if (!sid) {
      const info = await sessions.createSession('plan')
      sid = info.id
      sessions.selectSession(sid)
      router.replace(`/plan/${sid}`)
    }
    generatingText.value = 'Generating plan...'
    thinkingOutput.value = ''
    await plan.generatePlan(sid, g, (text) => {
      thinkingOutput.value += text
    })
  } finally {
    generating.value = false
    generatingText.value = ''
  }
}

async function handlePreReplan() {
  const fb = preExecFeedback.value.trim()
  if (!fb) return
  replanning.value = true
  try {
    const sid = sessionId.value!
    // Re-generate with feedback appended to the original goal
    const goalWithFeedback = `${plan.planDef!.goal}\n\nUser feedback on current plan: ${fb}\n\nPlease regenerate the plan incorporating this feedback.`
    generatingText.value = 'Replanning...'
    thinkingOutput.value = ''
    await plan.generatePlan(sid, goalWithFeedback, (text) => {
      thinkingOutput.value += text
    })
    showReplan.value = false
    preExecFeedback.value = ''
  } catch (e: any) {
    plan.planError = e.message
  } finally {
    replanning.value = false
    generatingText.value = ''
  }
}

async function handleExecute() { if (sessionId.value) await plan.executePlan(sessionId.value) }
async function handleCancel() { if (sessionId.value) await plan.cancelExecution(sessionId.value) }
async function handleRetry(sid: string) { if (sessionId.value) await plan.retryStep(sessionId.value, sid) }
async function handleReplan() {
  if (!sessionId.value || !feedback.value.trim()) return
  await plan.replan(sessionId.value, feedback.value)
  feedback.value = ''
}
function handleApprove(allowed: boolean, feedback?: string) {
  const sid = sessionId.value
  if (!sid) return
  plan.approveTool(sid, allowed, feedback)
}

onBeforeUnmount(() => { plan.clearPlan() })
</script>

<style scoped>
.plan-view { height: 100%; padding: 24px; }
.plan-create { max-width: 560px; margin: 60px auto; }
.goal-input { margin: 16px 0; }
.plan-header { display: flex; justify-content: space-between; align-items: center; width: 100%; }
.replan-alert { border-color: rgba(245,158,11,0.3); }
.gate-card { border-color: #22c55e; }
.retry-card { border-color: #ef4444; }
.thinking-box { margin-top: 12px; }
.thinking-text { font-size: 0.82em; white-space: pre-wrap; word-break: break-word; max-height: 300px; overflow-y: auto; background: rgba(255,255,255,0.04); padding: 10px; border-radius: 6px; }
</style>

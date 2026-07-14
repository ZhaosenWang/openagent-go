<template>
  <MessageStream
    :messages="chat.messages"
    :usage="chat.usage"
    :error="chat.error"
    :disabled="chat.streaming || !sessionId"
    :session-id="sessionId || undefined"
    @send="handleSend"
  />
  <ToolApprovalDialog
    v-if="chat.pendingApproval"
    :key="chat.pendingApproval.toolCall.id"
    :tool-call="chat.pendingApproval.toolCall"
    @resolve="handleApprove"
  />
</template>

<script setup lang="ts">
import { computed, watchEffect, onBeforeUnmount } from 'vue'
import { useRoute } from 'vue-router'
import { useChatStore } from '@/stores/chat'
import { useSessionsStore } from '@/stores/sessions'
import MessageStream from '@/components/chat/MessageStream.vue'
import ToolApprovalDialog from '@/components/chat/ToolApprovalDialog.vue'

const route = useRoute()
const chat = useChatStore()
const sessions = useSessionsStore()

const sessionId = computed(() => (route.params.sessionId as string) || sessions.currentSessionId)

watchEffect(() => {
  const sid = sessionId.value
  if (!sid) return
  chat.activateSession(sid, 'team')
})
onBeforeUnmount(() => { chat.clearChat() })

function handleSend(text: string) {
  const sid = sessionId.value
  if (!sid) return
  chat.sendMessage(sid, text, 'team')
}
function handleApprove(allowed: boolean, feedback?: string) {
  const sid = sessionId.value
  if (!sid) return
  chat.approveTool(sid, 'team', allowed, feedback)
}
</script>

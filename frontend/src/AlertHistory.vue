<script setup lang="ts">
import { nextTick, ref } from 'vue'
import { useAlertHistory } from './useAlertHistory'
import type { MonitoringSessionAccess } from './useMonitoringSession'
import {
  alertTypes as types,
  alertStatuses as statuses,
  formatTemperature,
  receiptTime,
} from './monitoringPresentation'

const props = defineProps<{ session: MonitoringSessionAccess }>()
const emit = defineEmits<{ pending: [pending: boolean] }>()
const heading = ref<HTMLElement | null>(null)
const resultInput = ref<HTMLTextAreaElement | null>(null)

// DOM 更新後才移動焦點，且不讓已換頁的讀取搶走目前焦點。
async function focusHeading(stillCurrent?: () => boolean): Promise<void> {
  await nextTick()
  if (stillCurrent && !stillCurrent()) return
  heading.value?.focus()
}

const {
  page, offset, pageLoading, pageError, selectedId, selectedAlert, detailLoading,
  detailError, detailUnavailable, draft, validationError, closureMessage, closurePending,
  needsVerification, resultLength, liveTarget, liveReady,
  loadPage, loadAlert, refreshLive, reload, openAlert, returnToHistory, closeAlert,
} = useAlertHistory(props.session, {
  pending: pending => emit('pending', pending),
  focusHeading,
  focusResult: () => resultInput.value?.focus(),
})

defineExpose({ liveReady, liveTarget, needsVerification, refreshLive, reload })
</script>

<template>
  <section
    ref="heading"
    class="monitor-board alert-board"
    :class="{ 'has-selection': selectedId }"
    aria-label="告警歷程"
    tabindex="-1"
  >
    <div class="list-pane">
      <div class="table-heading alert-columns" aria-hidden="true">
        <span>告警</span>
        <span>設備編號</span>
        <span>狀態</span>
      </div>
      <p v-if="pageLoading" class="pane-message" role="status">正在載入告警歷程…</p>
      <p v-if="pageError" class="notice pane-message" role="alert">{{ pageError }}</p>
      <p
        v-else-if="!pageLoading && !page?.items.length"
        class="muted pane-message"
      >目前沒有可查看的告警。</p>
      <ul v-if="page?.items.length" class="device-list alert-list">
        <li v-for="alert in page.items" :key="alert.id">
          <button
            type="button"
            class="device-button alert-button alert-columns"
            :aria-pressed="selectedId === alert.id"
            :disabled="
              pageLoading
              || detailLoading
              || closurePending
              || needsVerification
            "
            @click="openAlert(alert.id)"
          >
            <span class="alert-name">#{{ alert.id }} {{ types[alert.type] }}</span>
            <span class="alert-device">{{ alert.deviceId }}</span>
            <span class="status" :class="alert.status">{{ statuses[alert.status] }}</span>
          </button>
        </li>
      </ul>
      <footer class="list-footer">
        <p v-if="page" class="muted">共 {{ page.total }} 筆告警</p>
        <nav
          v-if="page && page.total > page.limit"
          class="pagination alert-pagination"
          aria-label="告警歷程分頁"
        >
          <button
            type="button"
            class="button button-secondary"
            :disabled="
              pageLoading
              || !!selectedId
              || offset === 0
            "
            @click="loadPage(Math.max(0, offset - page.limit))"
          >上一頁</button>
          <span aria-live="polite">第 {{ Math.floor(offset / page.limit) + 1 }} / {{ Math.ceil(page.total / page.limit) }} 頁</span>
          <button
            type="button"
            class="button button-secondary"
            :disabled="
              pageLoading
              || !!selectedId
              || offset + page.limit >= page.total
            "
            @click="loadPage(offset + page.limit)"
          >下一頁</button>
        </nav>
      </footer>
    </div>

    <section class="detail-pane alert-detail" aria-labelledby="alert-history-title">
      <template v-if="selectedId">
        <button
          type="button"
          class="back-button"
          :disabled="closurePending"
          @click="returnToHistory"
        >← 返回告警歷程</button>
        <div class="detail-heading">
          <div>
            <h2 id="alert-history-title">告警 #{{ selectedId }}</h2>
            <p v-if="selectedAlert" class="muted">{{ selectedAlert.deviceId }} · {{ types[selectedAlert.type] }}</p>
          </div>
          <span
            v-if="selectedAlert"
            class="status"
            :class="selectedAlert.status"
          >{{ statuses[selectedAlert.status] }}</span>
        </div>
        <p v-if="detailLoading" role="status">正在讀取告警…</p>
        <p v-else-if="closurePending" role="status">正在結案…</p>
        <p v-if="closureMessage" class="closure-message" role="status">{{ closureMessage }}</p>
        <p v-if="detailError" class="notice" role="alert">{{ detailError }}</p>
        <button
          v-if="detailError && !detailUnavailable"
          type="button"
          class="button button-secondary"
          :disabled="closurePending || detailLoading"
          @click="loadAlert()"
        >重新讀取告警</button>

        <dl v-if="selectedAlert" class="device-details alert-details">
          <div class="timeline-event completed">
            <dt>發現時間</dt>
            <dd>
              <time :datetime="selectedAlert.detectedAt">{{ receiptTime.format(new Date(selectedAlert.detectedAt)) }}</time>
            </dd>
          </div>
          <div class="timeline-event" :class="{ completed: selectedAlert.recoveredAt }">
            <dt>恢復時間</dt>
            <dd>
              <time
                v-if="selectedAlert.recoveredAt"
                :datetime="selectedAlert.recoveredAt"
              >{{ receiptTime.format(new Date(selectedAlert.recoveredAt)) }}</time>
              <template v-else>尚未恢復</template>
            </dd>
          </div>
          <div class="timeline-event timeline-last" :class="{ completed: selectedAlert.closure }">
            <dt>{{ selectedAlert.closure ? '結案時間' : '待結案' }}</dt>
            <dd>
              <time
                v-if="selectedAlert.closure"
                :datetime="selectedAlert.closure.closedAt"
              >{{ receiptTime.format(new Date(selectedAlert.closure.closedAt)) }}</time>
              <span v-else-if="selectedAlert.status === 'active'" class="muted">異常恢復後才能結案</span>
            </dd>
          </div>
          <div v-if="selectedAlert.temperature !== undefined" class="evidence-row">
            <dt>異常溫度</dt>
            <dd class="abnormal">{{ formatTemperature(selectedAlert.temperature) }}°C</dd>
          </div>
          <div v-if="selectedAlert.deadlineAt" class="evidence-row">
            <dt>離線期限</dt>
            <dd>
              <time :datetime="selectedAlert.deadlineAt">{{ receiptTime.format(new Date(selectedAlert.deadlineAt)) }}</time>
            </dd>
          </div>
          <div v-if="selectedAlert.detectedAtRestart !== undefined" class="evidence-row">
            <dt>重啟補判</dt>
            <dd>{{ selectedAlert.detectedAtRestart ? '是' : '否' }}</dd>
          </div>
          <template v-if="selectedAlert.closure">
            <div class="evidence-row">
              <dt>結案人</dt>
              <dd>{{ selectedAlert.closure.actor }}</dd>
            </div>
            <div class="closure-record">
              <dt>處理結果</dt>
              <dd class="closure-result">{{ selectedAlert.closure.result }}</dd>
            </div>
          </template>
        </dl>

        <form
          v-if="selectedAlert?.status === 'recovered'"
          class="closure-form"
          :aria-busy="closurePending"
          @submit.prevent="closeAlert"
        >
          <label for="closure-result">處理結果</label>
          <p id="closure-result-help" class="muted">輸入 1～2,000 個字元的非空白純文字，可換行。已輸入 {{ resultLength }} 個字元。</p>
          <textarea
            id="closure-result"
            ref="resultInput"
            v-model="draft"
            rows="5"
            required
            :aria-describedby="validationError ? 'closure-result-help closure-result-error' : 'closure-result-help'"
            :aria-invalid="validationError ? 'true' : undefined"
            :readonly="closurePending || needsVerification"
            @input="validationError = ''"
          ></textarea>
          <p
            v-if="validationError"
            id="closure-result-error"
            class="notice"
            role="alert"
          >{{ validationError }}</p>
          <button
            type="submit"
            class="button"
            :disabled="
              closurePending
              || detailLoading
              || needsVerification
            "
          >結案</button>
          <p class="muted closure-help">結案後，處理人、時間與結果將保留於歷程。</p>
        </form>
      </template>
      <div v-else class="empty-detail">
        <svg viewBox="0 0 32 32" aria-hidden="true">
          <rect x="7" y="3" width="18" height="26" rx="2" />
          <path d="M12 10h8M12 16h8M12 22h5" />
        </svg>
        <h2 id="alert-history-title">選擇一筆告警</h2>
        <p class="muted">查看異常、恢復時間與處理結果。</p>
      </div>
    </section>
  </section>
</template>

<style scoped>
.alert-board {
  grid-template-columns: minmax(0, 1.18fr) minmax(0, 1fr);
}
.alert-columns {
  display: grid;
  grid-template-columns: minmax(0, 1.15fr) minmax(0, 1fr) minmax(0, 1fr);
  align-items: center;
  gap: 20px;
}
.alert-button {
  min-height: 94px;
  overflow-wrap: anywhere;
}
.alert-name {
  font-weight: 700;
}
.alert-detail .detail-heading {
  flex-wrap: wrap;
}
.alert-detail .detail-heading .status {
  max-width: 100%;
  white-space: normal;
}
.alert-details {
  padding-top: 26px;
}
.timeline-event {
  position: relative;
  min-height: 82px;
  margin-left: 11px;
  padding: 0 0 22px 32px;
  border-left: 2px solid #cbd4df;
}
.timeline-event::before {
  position: absolute;
  left: -10px;
  top: 3px;
  width: 18px;
  height: 18px;
  border: 2px solid #aebac9;
  border-radius: 50%;
  background: #fff;
  content: "";
}
.timeline-event.completed::before {
  border-color: #245edb;
  background: #245edb;
}
.timeline-event dt {
  color: #101923;
  font-size: 19px;
  font-weight: 700;
}
.timeline-event dd {
  margin-top: 4px;
  color: #566170;
  font-size: 16px;
}
.timeline-last {
  min-height: 58px;
  margin-bottom: 20px;
  padding-bottom: 0;
  border-color: transparent;
}
.evidence-row {
  display: grid;
  grid-template-columns: 130px minmax(0, 1fr);
  align-items: center;
  gap: 14px;
  margin-top: 10px;
  padding: 10px 14px;
  border: 1px solid #e0e6ed;
  border-radius: 5px;
  background: #f7f9fc;
  font-size: 15px;
}
.evidence-row dt {
  font-weight: 650;
}
.evidence-row .abnormal {
  font-size: 24px;
  font-weight: 650;
}
.closure-form, .closure-record {
  margin-top: 26px;
  padding-top: 22px;
  border-top: 1px solid #cbd4df;
}
.closure-form label, .closure-record dt {
  margin: 0;
  color: #101923;
  font-size: 23px;
  font-weight: 700;
}
.closure-form p {
  margin-bottom: 4px;
  font-size: 14px;
}
.closure-form textarea {
  min-height: 140px;
  font: inherit;
  line-height: 1.6;
  resize: vertical;
}
.closure-form .button {
  margin-top: 8px;
  font-size: 19px;
}
.closure-form .closure-help {
  margin-top: 3px;
}
.closure-record dd {
  margin-top: 16px;
  line-height: 1.75;
}
.closure-result {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.closure-message {
  margin: 16px 0;
  color: #245edb;
}
@media (max-width: 1200px) {
  .alert-columns {
    gap: 12px;
  }
  .evidence-row {
    grid-template-columns: 90px minmax(0, 1fr);
  }
}
@media (max-width: 960px) {
  .alert-board {
    grid-template-columns: 1fr;
  }
}
@media (max-width: 600px) {
  .alert-columns {
    grid-template-columns: minmax(0, 1fr) minmax(110px, .8fr);
    gap: 8px;
  }
  .alert-button {
    min-height: 108px;
  }
  .alert-name {
    grid-column: 1;
    grid-row: 1;
  }
  .alert-device {
    grid-column: 1;
    grid-row: 2;
    font-size: 14px;
  }
  .alert-button .status {
    grid-column: 2;
    grid-row: 1 / 3;
    font-size: 14px;
  }
  .alert-detail .detail-heading {
    align-items: flex-start;
  }
  .evidence-row {
    grid-template-columns: 80px minmax(0, 1fr);
    padding: 10px;
    gap: 10px;
    font-size: 14px;
  }
  .timeline-event {
    padding-left: 25px;
  }
  .timeline-event dd {
    font-size: 14px;
  }
}
</style>

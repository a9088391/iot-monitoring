<script setup lang="ts">
import { formatTemperature, receiptTime } from './monitoringPresentation'
import type { useTaichungMonitoring } from './useTaichungMonitoring'

// 狀態由登入工作區持有，元件只顯示同一官方觀測與目前選取。
const { monitoring } = defineProps<{ monitoring: ReturnType<typeof useTaichungMonitoring> }>()
const { snapshot, selectedId, selected, loading, error } = monitoring
</script>

<template>
  <div class="official-monitoring">
    <div class="official-source-note">
      <p><strong>臺中市政府公開資料</strong> · 隨機選取最多六台環境感測器</p>
      <p class="muted">官方資料約每十分鐘更新，本站每分鐘檢查目前設備。請以原量測時間為準，重新抽選不一定會有新數值。</p>
      <p v-if="snapshot" class="muted official-fetch-time">
        來源快照擷取時間：<time :datetime="snapshot.fetchedAt">{{ receiptTime.format(new Date(snapshot.fetchedAt)) }}</time> ·
        <a :href="snapshot.sourceUrl" target="_blank" rel="noopener noreferrer">官方 JSON 來源</a>
      </p>
      <p class="muted">
        <a href="https://data.gov.tw/dataset/152428" target="_blank" rel="noopener noreferrer">政府資料開放平臺資料集</a> ·
        政府資料開放授權條款第 1 版。
      </p>
    </div>
    <div v-if="error" class="official-error" role="alert">
      <p class="notice">{{ error }}{{ snapshot ? ' 以下保留上次成功快照，並非本次更新結果；請核對各筆量測時間。' : ' 目前沒有可顯示的官方資料。' }}</p>
      <button
        type="button"
        class="button button-secondary"
        @click="monitoring.load()"
      >重試官方資料</button>
    </div>
    <p v-if="loading" role="status">正在讀取官方資料…</p>
    <p
      v-if="snapshot && snapshot.items.length < 6"
      class="muted official-count-note"
    >目前顯示 {{ snapshot.items.length }} 台 / 來源有效設備 {{ snapshot.total }} 台；來源不足六台，或原選設備可能已無資料。</p>
    <section
      class="official-board"
      :class="{ 'has-official-selection': selectedId }"
      aria-label="臺中市官方感測監控"
      :aria-busy="loading"
    >
      <div class="official-list-pane">
        <div class="official-columns official-table-heading" aria-hidden="true">
          <span>行政區與地點</span>
          <span>溫度</span>
        </div>
        <p
          v-if="snapshot && !snapshot.items.length"
          class="pane-message muted"
        >目前沒有可顯示的官方感測資料。請稍後重新抽選。</p>
        <ul class="official-device-list">
          <li v-for="sensor in snapshot?.items ?? []" :key="sensor.id">
            <button
              type="button"
              class="official-device-button official-columns"
              :aria-pressed="selectedId === sensor.id"
              :data-sensor-id="sensor.id"
              @click="selectedId = sensor.id"
            >
              <span class="official-sensor-name">
                {{ sensor.name }}
                <small>{{ sensor.device }} · {{ sensor.id }}</small>
              </span>
              <span class="official-row-temperature">{{ formatTemperature(sensor.temperature) }}°C</span>
              <time
                class="official-row-time"
                :datetime="sensor.measuredAt"
              >量測：{{ receiptTime.format(new Date(sensor.measuredAt)) }}</time>
            </button>
          </li>
        </ul>
        <p v-if="snapshot" class="official-list-footer muted">本次 {{ snapshot.items.length }} 台 / 來源 {{ snapshot.total }} 台有效設備</p>
      </div>
      <section class="official-detail-pane" aria-labelledby="official-detail-title">
        <template v-if="selected">
          <button
            type="button"
            class="back-button official-back-button"
            @click="selectedId = null"
          >← 返回官方設備清單</button>
          <h2 id="official-detail-title">{{ selected.name }}</h2>
          <p class="muted official-device-identity">{{ selected.device }} · {{ selected.id }}</p>
          <dl class="official-details">
            <div class="official-temperature">
              <dt>來源量測溫度</dt>
              <dd>{{ formatTemperature(selected.temperature) }}<small>°C</small></dd>
            </div>
            <div>
              <dt>相對濕度</dt>
              <dd>{{ selected.humidity === null ? '來源未提供有效值' : `${selected.humidity}%` }}</dd>
            </div>
            <div>
              <dt>PM2.5</dt>
              <dd>{{ selected.pm25 === null ? '來源未提供有效值' : `${selected.pm25} μg/m³` }}</dd>
            </div>
            <div>
              <dt>來源量測時間</dt>
              <dd><time :datetime="selected.measuredAt">{{ receiptTime.format(new Date(selected.measuredAt)) }}</time></dd>
            </div>
          </dl>
          <p class="muted">官方觀測資料；告警情境請使用「模擬展示」。</p>
        </template>
        <div v-else class="empty-detail">
          <h2 id="official-detail-title">選擇一台官方設備</h2>
          <p class="muted">查看同一筆觀測的溫度、濕度、PM2.5 與量測時間。</p>
        </div>
      </section>
    </section>
    <p class="workspace-note muted">重新抽選或重新整理瀏覽器會再隨機取樣，設備可能與上次重疊。告警與操作情境請進入「模擬展示」。</p>
  </div>
</template>

<style scoped>
.official-source-note p {
  margin-bottom: 8px;
}
.official-source-note {
  margin-bottom: 20px;
}
.official-source-note a {
  color: #245edb;
  overflow-wrap: anywhere;
}
.official-error {
  margin-bottom: 20px;
}
.official-error p {
  margin-bottom: 10px;
}
.official-board {
  display: grid;
  grid-template-columns: minmax(0, 1.2fr) minmax(0, 1fr);
  border-radius: 8px;
  background: #fff;
  overflow: clip;
}
.official-list-pane {
  min-width: 0;
}
.official-columns {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 8px 20px;
  align-items: center;
}
.official-table-heading {
  min-height: 66px;
  padding: 20px 24px;
  font-size: 20px;
  font-weight: 650;
  border-bottom: 1px solid #cbd4df;
}
.official-device-list {
  list-style: none;
  margin: 0;
  padding: 0;
}
.official-device-list li {
  border-bottom: 1px solid #cbd4df;
}
.official-device-button {
  width: 100%;
  padding: 19px 24px;
  border: 0;
  background: #fff;
  color: inherit;
  text-align: left;
}
.official-device-button:hover {
  background: #f1f6fd;
}
.official-device-button[aria-pressed="true"] {
  background: #e1edff;
  box-shadow: inset 5px 0 #245edb;
}
.official-sensor-name {
  font-size: 22px;
  font-weight: 700;
  overflow-wrap: anywhere;
}
.official-sensor-name small {
  display: block;
  margin-top: 4px;
  font-size: 14px;
  font-weight: 400;
  color: #566170;
}
.official-row-temperature {
  font-size: 30px;
}
.official-row-time {
  grid-column: 1 / -1;
  color: #566170;
  font-size: 14px;
}
.official-list-footer {
  padding: 18px 24px;
  margin: 0;
}
.official-detail-pane {
  min-width: 0;
  padding: 28px;
  border-left: 1px solid #cbd4df;
  overflow-wrap: anywhere;
}
.official-detail-pane h2 {
  font-size: 27px;
}
.official-device-identity {
  margin-top: 10px;
}
.official-details {
  margin: 28px 0;
}
.official-details > div {
  padding: 14px 0;
  border-bottom: 1px solid #e2e7ed;
}
.official-details dt {
  color: #566170;
  margin-bottom: 8px;
}
.official-details dd {
  margin: 0;
  font-size: 20px;
}
.official-temperature dd {
  color: #245edb;
  font-size: 68px;
  line-height: 1.2;
  font-weight: 700;
}
.official-temperature small {
  margin-left: 8px;
  font-size: 25px;
  font-weight: 400;
}
.official-back-button {
  display: none;
}
@media (max-width: 1100px) {
  .official-board {
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  }
  .official-device-button, .official-table-heading {
    padding-inline: 18px;
  }
  .official-row-temperature {
    font-size: 25px;
  }
  .official-sensor-name {
    font-size: 20px;
  }
}
@media (max-width: 760px) {
  .official-board {
    display: block;
  }
  .official-detail-pane {
    display: none;
    border: 0;
    padding: 24px;
  }
  .has-official-selection .official-list-pane {
    display: none;
  }
  .has-official-selection .official-detail-pane {
    display: block;
  }
  .official-back-button {
    display: block;
  }
}
</style>

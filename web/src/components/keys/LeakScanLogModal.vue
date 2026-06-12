<script setup lang="ts">
import { leakScanApi } from "@/api/leak-scan";
import type { GroupLeakScanEvent, GroupLeakScanRun } from "@/types/models";
import { Close } from "@vicons/ionicons5";
import { NButton, NCard, NEmpty, NIcon, NModal, NSpin, NTag } from "naive-ui";
import { watch, ref } from "vue";

const props = defineProps<{ show: boolean; groupId?: number }>();
const emit = defineEmits<{ (e: "update:show", value: boolean): void }>();

const loading = ref(false);
const loadingMore = ref(false);
const run = ref<GroupLeakScanRun | undefined>();
const events = ref<GroupLeakScanEvent[]>([]);
const page = ref(1);
const totalPages = ref(0);
const pageSize = 30;

watch(
  () => props.show,
  show => {
    if (show) load();
  }
);

async function load() {
  if (!props.groupId) return;
  loading.value = true;
  try {
    page.value = 1;
    const res = await leakScanApi.events(props.groupId, { page: page.value, page_size: pageSize });
    run.value = res.run;
    events.value = res.events || [];
    totalPages.value = res.pagination?.total_pages || 0;
  } finally {
    loading.value = false;
  }
}

async function loadMore() {
  if (!props.groupId || loadingMore.value || page.value >= totalPages.value) return;
  loadingMore.value = true;
  try {
    page.value += 1;
    const res = await leakScanApi.events(props.groupId, { page: page.value, page_size: pageSize });
    events.value = [...events.value, ...(res.events || [])];
    totalPages.value = res.pagination?.total_pages || totalPages.value;
  } finally {
    loadingMore.value = false;
  }
}
</script>

<template>
  <n-modal :show="show" @update:show="(value: boolean) => emit('update:show', value)" class="leak-log-modal">
    <n-card
      class="leak-log-card"
      title="泄露扫描任务日志"
      :bordered="false"
      size="huge"
      role="dialog"
      aria-modal="true"
    >
      <template #header-extra>
        <n-button quaternary circle @click="emit('update:show', false)">
          <template #icon>
            <n-icon :component="Close" />
          </template>
        </n-button>
      </template>

      <n-spin :show="loading">
        <div class="summary" v-if="run">
          <div><span>状态</span><strong>{{ run.status }}</strong></div>
          <div><span>搜索结果</span><strong>{{ run.expected_search_items }}</strong></div>
          <div><span>页数</span><strong>{{ run.processed_pages }} / {{ run.expected_pages }}</strong></div>
          <div><span>识别候选</span><strong>{{ run.collected_count }}</strong></div>
          <div><span>重复</span><strong>{{ run.duplicate_count }}</strong></div>
          <div><span>通过</span><strong>{{ run.valid_count }}</strong></div>
          <div><span>失败</span><strong>{{ run.invalid_count }}</strong></div>
          <div><span>写入有效</span><strong>{{ run.imported_count }}</strong></div>
        </div>
        <n-empty v-if="!events.length" description="暂无日志" />
        <div v-else class="event-list">
          <div v-for="event in events" :key="event.id" class="event-item">
            <n-tag size="small" :type="event.level === 'error' ? 'error' : 'info'">{{ event.event_type }}</n-tag>
            <div class="event-main">
              <div>{{ event.message }}</div>
              <small>{{ event.created_at }}</small>
              <pre v-if="event.payload">{{ JSON.stringify(event.payload, null, 2) }}</pre>
            </div>
          </div>
          <n-button v-if="page < totalPages" block :loading="loadingMore" @click="loadMore">
            加载更多
          </n-button>
        </div>
      </n-spin>

      <template #footer>
        <div class="actions">
          <n-button @click="load">刷新</n-button>
          <n-button type="primary" @click="emit('update:show', false)">关闭</n-button>
        </div>
      </template>
    </n-card>
  </n-modal>
</template>

<style scoped>
.leak-log-modal { width: 860px; }
.leak-log-card :deep(.n-card-header) { border-bottom: 1px solid var(--border-color); padding: 10px 20px; }
.leak-log-card :deep(.n-card__content) { max-height: calc(100vh - 68px - 61px - 50px); overflow-y: auto; padding-top: 20px; }
.leak-log-card :deep(.n-card__footer) { border-top: 1px solid var(--border-color); padding: 10px 15px; }
.summary { display: grid; grid-template-columns: repeat(4, 1fr); gap: 10px; margin-top: 20px; margin-bottom: 16px; }
.summary > div { border: 1px solid var(--border-color); border-radius: 8px; padding: 10px; display: flex; flex-direction: column; gap: 4px; }
.summary span, small { color: var(--text-secondary); font-size: 12px; }
.event-list { display: flex; flex-direction: column; gap: 10px; max-height: 520px; overflow: auto; }
.event-item { display: flex; gap: 10px; border-bottom: 1px solid var(--border-color); padding-bottom: 10px; }
.event-main { flex: 1; min-width: 0; }
pre { background: var(--code-bg-color, #f6f8fa); border-radius: 6px; padding: 8px; overflow: auto; font-size: 12px; }
.actions { display: flex; justify-content: flex-end; gap: 10px; }
@media (max-width: 760px) { .summary { grid-template-columns: repeat(2, 1fr); } }
</style>

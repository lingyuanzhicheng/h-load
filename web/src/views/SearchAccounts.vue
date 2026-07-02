<script setup lang="ts">
import { searchAccountsApi } from "@/api/search-accounts";
import githubIcon from "@/assets/github.svg";
import type { GitHubSearchAccount, SearchAccountStatus, SearchAccountType } from "@/types/models";
import { copy } from "@/utils/clipboard";
import {
  Add,
  AddCircleOutline,
  AlertCircleOutline,
  CheckmarkCircle,
  Close,
  CopyOutline,
  EyeOffOutline,
  EyeOutline,
  HelpCircleOutline,
  Pencil,
  Remove,
  RemoveCircleOutline,
} from "@vicons/ionicons5";
import {
  NButton,
  NCard,
  NDropdown,
  NForm,
  NFormItem,
  NIcon,
  NInput,
  NInputNumber,
  NModal,
  NSelect,
  NSpin,
  NTooltip,
  useDialog,
} from "naive-ui";
import { computed, onMounted, reactive, ref, watch } from "vue";

interface AccountTypeItem {
  type: SearchAccountType;
  title: string;
}

interface UserAgentItem {
  value: string;
  weight: number;
}

interface AccountRow extends GitHubSearchAccount {
  is_visible: boolean;
}

const accountTypes: AccountTypeItem[] = [
  { type: "github_api", title: "GitHub API" },
  { type: "github_web", title: "GitHub Web" },
];

const accounts = ref<AccountRow[]>([]);
const selectedType = ref<SearchAccountType>("github_api");
const loading = ref(false);
const modalShow = ref(false);
const editing = ref<GitHubSearchAccount | null>(null);
const statusFilter = ref<"all" | SearchAccountStatus>("all");
const currentPage = ref(1);
const pageSize = ref(12);
const dialog = useDialog();

const form = reactive<{ credential: string; user_agents: UserAgentItem[] }>({
  credential: "",
  user_agents: [{ value: navigator.userAgent || "Mozilla/5.0", weight: 1 }],
});

const statusOptions = [
  { label: "全部", value: "all" },
  { label: "有效", value: "active" },
  { label: "无效", value: "inactive" },
  { label: "受限", value: "limited" },
];

const moreOptions = [
  { label: "导出全部账户", key: "copyAll" },
  { label: "导出有效账户", key: "copyActive" },
  { label: "导出无效账户", key: "copyInactive" },
  { label: "导出受限账户", key: "copyLimited" },
  { type: "divider" },
  { label: "验证全部账户", key: "validateAll" },
  { label: "验证有效账户", key: "validateActive" },
  { label: "验证无效账户", key: "validateInactive" },
  { label: "验证受限账户", key: "validateLimited" },
  { type: "divider" },
  {
    label: "清空无效账户",
    key: "clearInactive",
    props: { style: { color: "#d03050" } },
  },
  {
    label: "清空受限账户",
    key: "clearLimited",
    props: { style: { color: "#d03050" } },
  },
  {
    label: "清空全部账户",
    key: "clearAll",
    props: { style: { color: "red", fontWeight: "bold" } },
  },
];

const typeCounts = computed(() => {
  const result: Record<SearchAccountType, number> = { github_api: 0, github_web: 0 };
  accounts.value.forEach(account => {
    result[account.type] += 1;
  });
  return result;
});

const selectedTypeTitle = computed(
  () => accountTypes.find(item => item.type === selectedType.value)?.title || "GitHub API"
);

const filteredAccounts = computed(() =>
  accounts.value.filter(
    account => account.type === selectedType.value && (statusFilter.value === "all" || account.status === statusFilter.value)
  )
);

const total = computed(() => filteredAccounts.value.length);
const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)));
const pagedAccounts = computed(() => {
  const start = (currentPage.value - 1) * pageSize.value;
  return filteredAccounts.value.slice(start, start + pageSize.value);
});

onMounted(loadAccounts);

watch([selectedType, statusFilter], () => {
  currentPage.value = 1;
});

async function loadAccounts() {
  loading.value = true;
  try {
    accounts.value = (await searchAccountsApi.list()).map(account => ({ ...account, is_visible: false }));
  } finally {
    loading.value = false;
  }
}

function selectType(type: SearchAccountType) {
  selectedType.value = type;
}

function openCreate() {
  editing.value = null;
  form.credential = "";
  form.user_agents = [{ value: navigator.userAgent || "Mozilla/5.0", weight: 1 }];
  modalShow.value = true;
}

function openEdit(account: GitHubSearchAccount) {
  editing.value = account;
  form.credential = account.credential;
  form.user_agents = parseUserAgents(account.device_id);
  modalShow.value = true;
}

function parseUserAgents(value: string): UserAgentItem[] {
  const items = value
    .split("\n")
    .map(item => parseUserAgentLine(item))
    .filter(item => item.value);
  return items.length ? items : [{ value: navigator.userAgent || "Mozilla/5.0", weight: 1 }];
}

function parseUserAgentLine(value: string): UserAgentItem {
  const line = value.trim();
  const tabIndex = line.indexOf("\t");
  if (tabIndex > 0) {
    const weight = Number(line.slice(0, tabIndex));
    if (Number.isFinite(weight) && weight > 0) {
      return { weight, value: line.slice(tabIndex + 1).trim() };
    }
  }
  return { value: line, weight: 1 };
}

function serializeUserAgents(items: UserAgentItem[]) {
  return items
    .map(item => ({ value: item.value.trim(), weight: item.weight || 1 }))
    .filter(item => item.value)
    .map(item => `${item.weight}\t${item.value}`)
    .join("\n");
}

function validUserAgents() {
  return form.user_agents
    .map(item => ({ value: item.value.trim(), weight: item.weight || 1 }))
    .filter(item => item.value);
}

function addUserAgent() {
  form.user_agents.push({ value: "", weight: 1 });
}

function removeUserAgent(index: number) {
  if (form.user_agents.length <= 1) {
    window.$message.warning("至少保留一个 User-Agent");
    return;
  }
  form.user_agents.splice(index, 1);
}

async function submit() {
	const credential = form.credential.trim();
	const userAgents = validUserAgents();
	if ((!editing.value && !credential) || userAgents.length === 0) {
		window.$message.warning("请填写凭证和 User-Agent");
		return;
	}
	const payload: Partial<GitHubSearchAccount> = {
		device_id: serializeUserAgents(userAgents),
	};
	if (editing.value) {
		await searchAccountsApi.update(editing.value.id, payload);
	} else {
		payload.type = selectedType.value;
		payload.credential = credential;
		payload.status = "active";
		await searchAccountsApi.create(payload);
	}
  modalShow.value = false;
  await loadAccounts();
}

function deleteVisibleAccounts() {
  if (filteredAccounts.value.length === 0) {
    window.$message.info("当前没有可删除的账户");
    return;
  }
  const d = dialog.warning({
    title: "删除账户",
    content: `确认删除当前筛选下的 ${filteredAccounts.value.length} 个${selectedTypeTitle.value}账户？`,
    positiveText: "确认",
    negativeText: "取消",
    onPositiveClick: async () => {
      d.loading = true;
      await Promise.all(filteredAccounts.value.map(account => searchAccountsApi.delete(account.id)));
      await loadAccounts();
    },
  });
}

function deleteAccount(account: GitHubSearchAccount) {
  const d = dialog.warning({
    title: "删除账户",
    content: `确认删除 ${formatAccountTitle(account)}？`,
    positiveText: "确认",
    negativeText: "取消",
    onPositiveClick: async () => {
      d.loading = true;
      await searchAccountsApi.delete(account.id);
      await loadAccounts();
    },
  });
}

async function copyCredential(account: GitHubSearchAccount) {
  if (await copy(account.credential)) {
    window.$message.success("账户凭证已复制");
  } else {
    window.$message.error("复制失败");
  }
}

function toggleCredentialVisibility(account: AccountRow) {
  account.is_visible = !account.is_visible;
}

function getCredentialDisplayValue(account: AccountRow) {
  return account.is_visible ? account.credential : maskCredential(account.credential);
}

async function testAccount(account: GitHubSearchAccount) {
  loading.value = true;
  try {
    const result = await searchAccountsApi.validate(account.id);
    await loadAccounts();
    if (result.is_valid) {
      window.$message.success("账户测试通过");
    } else {
      window.$message.error("账户测试失败");
    }
  } finally {
    loading.value = false;
  }
}

async function handleMoreAction(key: string) {
  switch (key) {
    case "copyAll":
      await exportAccounts(accounts.value.filter(account => account.type === selectedType.value));
      break;
    case "copyActive":
      await exportAccounts(accounts.value.filter(account => account.type === selectedType.value && account.status === "active"));
      break;
    case "copyInactive":
      await exportAccounts(accounts.value.filter(account => account.type === selectedType.value && account.status === "inactive"));
      break;
    case "copyLimited":
      await exportAccounts(accounts.value.filter(account => account.type === selectedType.value && account.status === "limited"));
      break;
    case "validateAll":
      await validateAccounts();
      break;
    case "validateActive":
      await validateAccounts("active");
      break;
    case "validateInactive":
      await validateAccounts("inactive");
      break;
    case "validateLimited":
      await validateAccounts("limited");
      break;
    case "clearInactive":
      clearAccounts("inactive");
      break;
    case "clearLimited":
      clearAccounts("limited");
      break;
    case "clearAll":
      clearAccounts();
      break;
  }
}

function clearAccounts(status?: SearchAccountStatus) {
  const targets = accounts.value.filter(
    account => account.type === selectedType.value && (!status || account.status === status)
  );
  if (targets.length === 0) {
    window.$message.info("没有可清空的账户");
    return;
  }
  const d = dialog.warning({
    title: status === "inactive" ? "清空无效账户" : status === "limited" ? "清空受限账户" : "清空全部账户",
    content: `确认删除 ${targets.length} 个${selectedTypeTitle.value}账户？`,
    positiveText: "确认",
    negativeText: "取消",
    onPositiveClick: async () => {
      d.loading = true;
      await Promise.all(targets.map(account => searchAccountsApi.delete(account.id)));
      await loadAccounts();
    },
  });
}

async function validateAccounts(status?: SearchAccountStatus) {
  loading.value = true;
  try {
    const result = await searchAccountsApi.validateMany({ type: selectedType.value, status });
    await loadAccounts();
    window.$message.success(`验证完成：有效 ${result.valid} 个，无效 ${result.invalid} 个`);
  } finally {
    loading.value = false;
  }
}

async function exportAccounts(items: GitHubSearchAccount[]) {
  if (items.length === 0) {
    window.$message.info("没有可导出的账户");
    return;
  }
  const content = items.map(account => account.credential).join("\n");
  const blob = new Blob([content], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.setAttribute("download", `accounts-${selectedType.value}-${Date.now()}.txt`);
  document.body.appendChild(link);
  link.click();
  document.body.removeChild(link);
  URL.revokeObjectURL(url);
  window.$message.success(`已导出 ${items.length} 个账户`);
}

function formatAccountTitle(account: GitHubSearchAccount) {
  return `${account.type === "github_api" ? "GitHub API" : "GitHub Web"} #${account.username || account.id}`;
}

function formatAccountUsername(account: GitHubSearchAccount) {
  return account.username ? `#${account.username}` : `#${account.id}`;
}

function maskCredential(value: string) {
  if (value.length <= 12) {
    return value;
  }
  return `${value.slice(0, 6)}******${value.slice(-6)}`;
}

function formatRelativeTime(value?: string) {
  if (!value) {
    return "未使用";
  }
  const diff = Date.now() - new Date(value).getTime();
  if (diff < 60_000) {
    return "刚刚";
  }
  if (diff < 3_600_000) {
    return `${Math.floor(diff / 60_000)}分钟前`;
  }
  if (diff < 86_400_000) {
    return `${Math.floor(diff / 3_600_000)}小时前`;
  }
  return `${Math.floor(diff / 86_400_000)}天前`;
}

function changePage(page: number) {
  currentPage.value = Math.min(Math.max(page, 1), totalPages.value);
}

function changePageSize(size: number) {
  pageSize.value = size;
  currentPage.value = 1;
}
</script>

<template>
  <div class="accounts-container">
    <div class="sidebar">
      <div class="account-list-container">
        <n-card :bordered="false" size="small" class="account-list-card modern-card">
          <div class="accounts-section">
            <div class="accounts-list">
              <button
                v-for="item in accountTypes"
                :key="item.type"
                class="account-type-item"
                :class="{ active: selectedType === item.type }"
                @click="selectType(item.type)"
              >
                <div class="type-icon">
                  <img :src="githubIcon" alt="github" />
                </div>
                <div class="type-content">
                  <div class="type-name">{{ item.title }}</div>
                  <div class="type-meta">
                    <n-tag size="tiny" :bordered="true" class="github-tag">github</n-tag>
                    <span class="type-id">#{{ typeCounts[item.type] }}</span>
                  </div>
                </div>
              </button>
            </div>
          </div>
        </n-card>
      </div>
    </div>

    <div class="main-content">
      <div class="account-table-section">
        <div class="account-table-container">
          <div class="toolbar">
            <div class="toolbar-left">
              <n-button type="success" size="small" @click="openCreate">
                <template #icon>
                  <n-icon :component="AddCircleOutline" />
                </template>
                添加账户
              </n-button>
              <n-button type="error" size="small" @click="deleteVisibleAccounts">
                <template #icon>
                  <n-icon :component="RemoveCircleOutline" />
                </template>
                删除账户
              </n-button>
            </div>
            <div class="toolbar-right">
              <n-select
                v-model:value="statusFilter"
                :options="statusOptions"
                size="small"
                style="width: 120px"
              />
              <n-dropdown :options="moreOptions" trigger="click" @select="handleMoreAction">
                <n-button size="small" tertiary>
                  <template #icon>
                    <span style="font-size: 16px; font-weight: bold">⋯</span>
                  </template>
                </n-button>
              </n-dropdown>
            </div>
          </div>

          <div class="accounts-grid-container">
            <n-spin :show="loading">
              <div v-if="pagedAccounts.length === 0 && !loading" class="empty-container">
                <n-empty description="暂无账户" />
              </div>
              <div v-else class="accounts-grid">
                <div
                  v-for="account in pagedAccounts"
                  :key="account.id"
                  class="account-card"
                  :class="account.status === 'active' ? 'status-valid' : account.status === 'limited' ? 'status-limited' : 'status-invalid'"
                >
                  <div class="account-main">
                    <div class="account-section">
                      <n-tag v-if="account.status === 'active'" type="success" :bordered="false" round>
                        <template #icon>
                          <n-icon :component="CheckmarkCircle" />
                        </template>
                        有效
                      </n-tag>
                      <n-tag v-else-if="account.status === 'limited'" type="warning" :bordered="false" round>
                        <template #icon>
                          <n-icon :component="AlertCircleOutline" />
                        </template>
                        受限
                      </n-tag>
                      <n-tag v-else :bordered="false" round>
                        <template #icon>
                          <n-icon :component="AlertCircleOutline" />
                        </template>
                        无效
                      </n-tag>
                      <n-input class="account-text" :value="getCredentialDisplayValue(account)" readonly size="small" />
                      <div class="quick-actions">
                        <n-button size="tiny" text title="编辑账户" @click="openEdit(account)">
                          <template #icon>
                            <n-icon :component="Pencil" />
                          </template>
                        </n-button>
                        <n-button size="tiny" text title="显示/隐藏" @click="toggleCredentialVisibility(account)">
                          <template #icon>
                            <n-icon :component="account.is_visible ? EyeOffOutline : EyeOutline" />
                          </template>
                        </n-button>
                        <n-button size="tiny" text title="复制凭证" @click="copyCredential(account)">
                          <template #icon>
                            <n-icon :component="CopyOutline" />
                          </template>
                        </n-button>
                      </div>
                    </div>
                  </div>

                  <div class="account-bottom">
                    <div class="account-stats">
                      <span class="stat-item">请求 <strong>{{ account.request_count }}</strong></span>
                      <span class="stat-item">失败 <strong>{{ account.failure_count }}</strong></span>
                      <span class="stat-item">{{ formatRelativeTime(account.last_used_at) }}</span>
                      <span class="stat-item">{{ formatAccountUsername(account) }}</span>
                    </div>
                    <n-button-group class="account-actions">
                      <n-button round tertiary type="info" size="tiny" @click="testAccount(account)">测试</n-button>
                      <n-button round tertiary type="error" size="tiny" @click="deleteAccount(account)">删除</n-button>
                    </n-button-group>
                  </div>
                </div>
              </div>
            </n-spin>
          </div>

          <div class="pagination-container">
            <div class="pagination-info">
              <span>共 {{ total }} 条</span>
              <n-select
                v-model:value="pageSize"
                :options="[
                  { label: '12 条/页', value: 12 },
                  { label: '24 条/页', value: 24 },
                  { label: '60 条/页', value: 60 },
                  { label: '120 条/页', value: 120 },
                ]"
                size="small"
                style="width: 100px; margin-left: 12px"
                @update:value="changePageSize"
              />
            </div>
            <div class="pagination-controls">
              <n-button size="small" :disabled="currentPage <= 1" @click="changePage(currentPage - 1)">上一页</n-button>
              <span class="page-info">第 {{ currentPage }} / {{ totalPages }} 页</span>
              <n-button size="small" :disabled="currentPage >= totalPages" @click="changePage(currentPage + 1)">下一页</n-button>
            </div>
          </div>
        </div>
      </div>
    </div>

    <n-modal :show="modalShow" @update:show="modalShow = $event" class="account-form-modal">
      <n-card
        class="account-form-card"
        :title="editing ? '编辑账户' : '添加账户'"
        :bordered="false"
        size="huge"
        role="dialog"
        aria-modal="true"
      >
        <template #header-extra>
          <n-button quaternary circle @click="modalShow = false">
            <template #icon>
              <n-icon :component="Close" />
            </template>
          </n-button>
        </template>

        <n-form label-placement="left" label-width="120px" require-mark-placement="right-hanging" class="account-form">
          <div class="form-section">
            <h4 class="section-title">基础信息</h4>
			<n-form-item label="凭证" required>
              <template #label>
                <div class="form-label-with-tooltip">
                  凭证
                  <n-tooltip trigger="hover" placement="top">
                    <template #trigger>
                      <n-icon :component="HelpCircleOutline" class="help-icon" />
                    </template>
                    GitHub API 填写 Token，GitHub Web 填写 user_session Cookie 值。
                  </n-tooltip>
                </div>
              </template>
				<n-input
					v-model:value="form.credential"
					type="textarea"
					:rows="4"
					placeholder="GitHub Token 或 user_session"
					:disabled="!!editing"
				/>
			</n-form-item>
          </div>

          <div class="form-section" style="margin-top: 10px">
            <h4 class="section-title">User-Agent</h4>
            <n-form-item
              v-for="(userAgent, index) in form.user_agents"
              :key="index"
              :label="`UA ${index + 1}`"
              required
            >
              <template #label>
                <div class="form-label-with-tooltip">
                  UA {{ index + 1 }}
                  <n-tooltip trigger="hover" placement="top">
                    <template #trigger>
                      <n-icon :component="HelpCircleOutline" class="help-icon" />
                    </template>
                    GitHub 请求使用的 User-Agent，可配置多个并按权重随机选择。
                  </n-tooltip>
                </div>
              </template>
              <div class="user-agent-row">
                <div class="user-agent-value">
                  <n-input v-model:value="userAgent.value" placeholder="Mozilla/5.0 ..." />
                </div>
                <div class="user-agent-weight">
                  <span class="weight-label">权重</span>
                  <n-tooltip trigger="hover" placement="top" style="width: 100%">
                    <template #trigger>
                      <n-input-number
                        v-model:value="userAgent.weight"
                        :min="1"
                        :max="100"
                        placeholder="权重"
                        style="width: 100%"
                      />
                    </template>
                    按权重随机选择 User-Agent
                  </n-tooltip>
                </div>
                <div class="user-agent-actions">
                  <n-button
                    v-if="form.user_agents.length > 1"
                    type="error"
                    quaternary
                    circle
                    size="small"
                    @click="removeUserAgent(index)"
                  >
                    <template #icon>
                      <n-icon :component="Remove" />
                    </template>
                  </n-button>
                </div>
              </div>
            </n-form-item>
            <n-form-item>
              <n-button dashed style="width: 100%" @click="addUserAgent">
                <template #icon>
                  <n-icon :component="Add" />
                </template>
                添加 User-Agent
              </n-button>
            </n-form-item>
          </div>
        </n-form>

        <template #footer>
          <div class="modal-actions">
            <n-button @click="modalShow = false">取消</n-button>
            <n-button type="primary" @click="submit">保存</n-button>
          </div>
        </template>
      </n-card>
    </n-modal>
  </div>
</template>

<style scoped>
.account-list-card :deep(.n-card__content) {
  height: 100%;
}

.accounts-container {
  display: flex;
  flex-direction: column;
  gap: 8px;
  width: 100%;
}

.sidebar {
  width: 100%;
  flex-shrink: 0;
}

.main-content {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}

.account-table-section {
  flex: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.account-list-container,
.account-list-card {
  height: 100%;
}

.account-list-card {
  display: flex;
  flex-direction: column;
  background: var(--card-bg-solid);
}

.account-list-card:hover {
  transform: none;
  box-shadow: var(--shadow-lg);
}

.accounts-section {
  flex: 1;
  height: 100%;
  overflow: auto;
}

.accounts-list {
  display: flex;
  flex-direction: column;
  gap: 4px;
  width: 100%;
}

.account-type-item {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 8px;
  border-radius: 6px;
  cursor: pointer;
  transition: all 0.2s ease;
  border: 1px solid var(--border-color);
  color: var(--text-primary);
  background: transparent;
  text-align: left;
  box-sizing: border-box;
}

.account-type-item:hover {
  background: var(--bg-tertiary);
  border-color: var(--primary-color);
}

.account-type-item.active,
:root.dark .account-type-item.active {
  background: var(--primary-gradient);
  color: white;
  border-color: transparent;
  box-shadow: var(--shadow-md);
}

.type-icon {
  width: 28px;
  height: 28px;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--bg-secondary);
  border-radius: 6px;
  flex-shrink: 0;
}

.type-icon img {
  width: 18px;
  height: 18px;
  display: block;
}

.account-type-item.active .type-icon {
  background: rgba(255, 255, 255, 0.2);
}

.type-content {
  flex: 1;
  min-width: 0;
}

.type-name {
  font-weight: 600;
  font-size: 14px;
  line-height: 1.2;
  margin-bottom: 4px;
}

.type-meta {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 10px;
  flex-wrap: wrap;
}

.github-tag {
  color: #4b5563;
  border-color: #4b5563;
  background: rgba(75, 85, 99, 0.08);
}

.type-id {
  color: var(--text-secondary);
  opacity: 0.8;
}

.account-type-item.active .type-id,
.account-type-item.active .github-tag {
  color: white;
  opacity: 0.9;
}

.account-type-item.active .github-tag {
  border-color: rgba(255, 255, 255, 0.35);
  background: rgba(255, 255, 255, 0.2);
}

.account-table-container {
  background: var(--card-bg-solid);
  border-radius: 8px;
  box-shadow: var(--shadow-md);
  border: 1px solid var(--border-color);
  overflow: hidden;
  height: 100%;
  display: flex;
  flex-direction: column;
}

.toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 16px;
  background: var(--card-bg-solid);
  border-bottom: 1px solid var(--border-color);
  flex-shrink: 0;
  gap: 16px;
  min-height: 64px;
}

.toolbar-left,
.toolbar-right {
  display: flex;
  align-items: center;
  gap: 8px;
}

.toolbar-right {
  justify-content: flex-end;
  flex: 1;
}

.accounts-grid-container {
  flex: 1;
  overflow-y: auto;
  padding: 16px;
}

.accounts-grid {
  display: grid;
  grid-template-columns: 1fr;
  gap: 16px;
}

.account-card {
  background: var(--card-bg-solid);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 14px;
  transition: all 0.2s;
  display: flex;
  flex-direction: column;
  gap: 10px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.05);
}

.account-card:hover {
  box-shadow: var(--shadow-md);
  transform: translateY(-1px);
}

.account-card.status-valid {
  border-color: var(--success-border);
  background: var(--success-bg);
  border-width: 1.5px;
}

.account-card.status-limited {
  border-color: #f0a020;
  background: rgba(240, 160, 32, 0.08);
  border-width: 1.5px;
}

.account-card.status-invalid {
  border-color: var(--invalid-border);
  background: var(--card-bg-solid);
  opacity: 0.85;
}

.account-main,
.account-section,
.account-bottom,
.account-stats {
  display: flex;
  align-items: center;
}

.account-main,
.account-bottom {
  justify-content: space-between;
  gap: 8px;
}

.account-section {
  gap: 8px;
  flex: 1;
  min-width: 0;
}

.account-text {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, Courier, monospace;
  font-weight: 500;
  flex: 1;
  min-width: 0;
  overflow: hidden;
  white-space: nowrap;
}

.quick-actions {
  display: flex;
  gap: 4px;
  flex-shrink: 0;
}

.account-stats {
  gap: 8px;
  font-size: 12px;
  overflow: hidden;
  color: var(--text-secondary);
  flex: 1;
  min-width: 0;
}

.stat-item {
  white-space: nowrap;
  color: var(--text-secondary);
}

.stat-item strong {
  color: var(--text-primary);
  font-weight: 600;
}

.account-actions {
  flex-shrink: 0;
}

.empty-container {
  padding: 40px 0;
}

.pagination-container {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 12px 16px;
  border-top: 1px solid var(--border-color);
  background: var(--card-bg-solid);
  flex-shrink: 0;
}

.pagination-info,
.pagination-controls {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  color: var(--text-secondary);
}

.page-info {
  padding: 0 8px;
  white-space: nowrap;
}

.account-form-modal {
  width: 800px;
}

.user-agent-row {
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
}

.user-agent-value {
  flex: 1;
}

.user-agent-weight {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 0 0 140px;
}

.weight-label {
  font-weight: 500;
  color: var(--text-primary);
  white-space: nowrap;
}

.user-agent-actions {
  flex: 0 0 32px;
  display: flex;
  justify-content: center;
}

.form-section {
  margin-top: 20px;
}

.section-title {
  font-size: 1rem;
  font-weight: 600;
  color: var(--text-primary);
  margin: 0 0 12px 0;
  padding-bottom: 8px;
  border-bottom: 2px solid var(--border-color);
}

.form-label-with-tooltip {
  display: flex;
  align-items: center;
  gap: 4px;
}

.help-icon {
  font-size: 14px;
  color: var(--text-secondary);
  cursor: help;
}

:deep(.n-form-item-label) {
  font-weight: 500;
}

:deep(.n-form-item-blank) {
  flex-grow: 1;
}

:deep(.n-input),
:deep(.n-select),
:deep(.n-input-number) {
  --n-border-radius: 6px;
}

.account-form-card :deep(.n-card-header) {
  border-bottom: 1px solid var(--border-color);
  padding: 10px 20px;
}

.account-form-card :deep(.n-card__content) {
  max-height: calc(100vh - 68px - 61px - 50px);
  overflow-y: auto;
  padding-top: 20px;
}

.account-form-card :deep(.n-card__footer) {
  border-top: 1px solid var(--border-color);
  padding: 10px 15px;
}

.account-form-card :deep(.n-form-item-feedback-wrapper) {
  min-height: 10px;
}

.modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}

@media (min-width: 768px) {
  .accounts-container {
    flex-direction: row;
  }

  .sidebar {
    width: 240px;
    height: calc(100vh - 159px);
  }
}

@media (max-width: 768px) {
  .account-form-modal {
    width: 100vw !important;
  }

  .toolbar,
  .pagination-container {
    align-items: stretch;
    flex-direction: column;
  }

  .toolbar-right,
  .toolbar-left,
  .pagination-controls,
  .pagination-info {
    justify-content: flex-start;
    width: 100%;
  }

  .user-agent-row {
    flex-direction: column;
    gap: 8px;
    align-items: stretch;
  }

  .user-agent-weight {
    flex: none;
  }
}
</style>

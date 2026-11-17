<template>
  <div>
    <div class="toolbar">
      <el-alert type="info" :closable="false" show-icon
        title="消息源自动获取 GitHub 仓库动态并转存到本站；订阅后新消息会实时推送到你的客户端。" />
      <div class="spacer" />
      <el-button type="primary" @click="openCreate">新建消息源</el-button>
    </div>

    <el-table :data="items" v-loading="loading" empty-text="还没有消息源">
      <el-table-column label="仓库" min-width="200">
        <template #default="{ row }">
          <div class="repo">
            <span class="repo-name">{{ row.owner }}/{{ row.repo }}</span>
            <span class="repo-topic">{{ row.topic }}</span>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="关注内容" min-width="220">
        <template #default="{ row }">
          <el-tag v-for="w in watchTags(row)" :key="w.label" size="small" :type="w.type" class="watch-tag">
            {{ w.label }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="Star / Fork" width="130">
        <template #default="{ row }">
          <span v-if="row.watch_stars || row.watch_forks">⭐ {{ row.last_stars }}　🍴 {{ row.last_forks }}</span>
          <span v-else class="dim">—</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="150">
        <template #default="{ row }">
          <el-tooltip v-if="row.last_error" :content="row.last_error" placement="top">
            <el-tag type="danger" size="small">拉取异常</el-tag>
          </el-tooltip>
          <el-tag v-else-if="!row.enabled" type="info" size="small">已停用</el-tag>
          <el-tag v-else type="success" size="small">正常</el-tag>
          <div class="dim run-at">{{ fmtRelTime(row.last_run_at) }}</div>
        </template>
      </el-table-column>
      <el-table-column label="订阅" width="100">
        <template #default="{ row }">
          <el-button v-if="row.subscribed" type="success" plain size="small" @click="toggleSub(row, false)">已订阅</el-button>
          <el-button v-else plain size="small" @click="toggleSub(row, true)">订阅</el-button>
        </template>
      </el-table-column>
      <el-table-column v-if="hasMine" label="管理" width="200">
        <template #default="{ row }">
          <template v-if="row.mine">
            <el-button text size="small" @click="openEdit(row)">编辑</el-button>
            <el-switch :model-value="row.enabled" size="small" @change="(v) => toggleEnabled(row, v)" />
            <el-button type="danger" text size="small" @click="remove(row)">删除</el-button>
          </template>
          <span v-else class="dim">—</span>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="createVisible" title="新建 GitHub 消息源" width="520px">
      <el-form label-width="110px">
        <el-form-item label="仓库">
          <div class="repo-input">
            <el-input v-model="form.owner" placeholder="owner，例如 linuxsuren" maxlength="100" />
            <span class="slash">/</span>
            <el-input v-model="form.repo" placeholder="repo，例如 message-hub" maxlength="100" />
          </div>
        </el-form-item>
        <el-form-item label="GitHub 令牌">
          <el-input v-model="form.token" type="password" show-password placeholder="ghp_…（仅创建时提交）" />
          <div class="hint">需要仓库读取与 webhook 管理权限；令牌加密存储，不会再次显示。</div>
        </el-form-item>
        <el-form-item label="关注内容">
          <el-checkbox-group v-model="form.watches">
            <el-checkbox v-for="opt in watchOptions" :key="opt.key" :value="opt.key">{{ opt.label }}</el-checkbox>
          </el-checkbox-group>
        </el-form-item>
        <el-form-item label="同步 webhook">
          <el-switch v-model="form.use_webhook" />
          <span class="hint inline">Release / Issue / PR 实时推送需要 GitHub 能回调本服务（公网地址），由你决定是否开启</span>
          <div v-if="form.use_webhook && !baseURLConfigured" class="hint warn">
            尚未配置对外地址，开启后无法注册；请先在「管理 → 系统设置」中配置
          </div>
        </el-form-item>
        <el-form-item label="轮询间隔">
          <el-input-number v-model="form.interval_sec" :min="60" :max="86400" :step="60" />
          <span class="hint inline">秒，star / fork 数变化的检查频率</span>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" :loading="creating" @click="create">创建并订阅</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="editVisible" :title="`编辑消息源 ${editForm.owner}/${editForm.repo}`" width="520px">
      <el-form label-width="110px">
        <el-form-item label="关注内容">
          <el-checkbox-group v-model="editForm.watches">
            <el-checkbox v-for="opt in watchOptions" :key="opt.key" :value="opt.key">{{ opt.label }}</el-checkbox>
          </el-checkbox-group>
        </el-form-item>
        <el-form-item label="同步 webhook">
          <el-switch v-model="editForm.use_webhook" />
          <span class="hint inline">当前：{{ editForm.webhook_ready ? '已注册' : '未注册' }}</span>
          <div v-if="editForm.use_webhook && !baseURLConfigured" class="hint warn">
            尚未配置对外地址，开启后无法注册；请先在「管理 → 系统设置」中配置
          </div>
        </el-form-item>
        <el-form-item label="轮询间隔">
          <el-input-number v-model="editForm.interval_sec" :min="60" :max="86400" :step="60" />
          <span class="hint inline">秒，star / fork 数变化的检查频率</span>
        </el-form-item>
        <el-form-item label="GitHub 令牌">
          <el-input v-model="editForm.token" type="password" show-password placeholder="留空则保持不变" />
          <div class="hint">令牌到期或权限变化时在此更换；填写后会先验证再保存。</div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="editVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="saveEdit">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, fmtRelTime } from '../api'

const watchOptions = [
  { key: 'watch_release', label: 'Release', type: 'primary' },
  { key: 'watch_issues', label: 'Issue', type: 'primary' },
  { key: 'watch_pulls', label: 'PR', type: 'primary' },
  { key: 'watch_stars', label: 'Star 数', type: 'warning' },
  { key: 'watch_forks', label: 'Fork 数', type: 'warning' },
]

const items = ref([])
const loading = ref(false)
const createVisible = ref(false)
const creating = ref(false)
const form = reactive({ owner: '', repo: '', token: '', watches: ['watch_release'], use_webhook: false, interval_sec: 300 })
const baseURLConfigured = ref(false)
const hasMine = computed(() => items.value.some((it) => it.mine))

// 编辑消息源：关注内容 / 轮询间隔 / 更换令牌（留空不变）
const editVisible = ref(false)
const saving = ref(false)
const editForm = reactive({ id: 0, owner: '', repo: '', token: '', watches: [], use_webhook: false, webhook_ready: false, interval_sec: 300 })

async function load() {
  loading.value = true
  try {
    const data = await api.get('/api/v1/sources')
    items.value = data.items || []
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    loading.value = false
  }
}

function watchTags(row) {
  const tags = watchOptions.filter((opt) => row[opt.key]).map((opt) => ({ label: opt.label, type: opt.type }))
  if (row.webhook_ready) tags.push({ label: 'Webhook', type: 'success' })
  return tags
}

function openCreate() {
  form.owner = ''
  form.repo = ''
  form.token = ''
  form.watches = ['watch_release']
  form.use_webhook = false
  form.interval_sec = 300
  createVisible.value = true
}

async function create() {
  if (!form.owner || !form.repo || !form.token) {
    ElMessage.warning('请填写仓库与 GitHub 令牌')
    return
  }
  if (form.watches.length === 0) {
    ElMessage.warning('请至少选择一项关注内容')
    return
  }
  creating.value = true
  try {
    const body = { owner: form.owner, repo: form.repo, token: form.token, interval_sec: form.interval_sec, use_webhook: form.use_webhook }
    form.watches.forEach((key) => (body[key] = true))
    await api.post('/api/v1/sources', body)
    ElMessage.success('消息源已创建，并自动为你订阅')
    createVisible.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    creating.value = false
  }
}

function openEdit(row) {
  editForm.id = row.id
  editForm.owner = row.owner
  editForm.repo = row.repo
  editForm.token = ''
  editForm.watches = watchOptions.filter((opt) => row[opt.key]).map((opt) => opt.key)
  editForm.use_webhook = row.use_webhook
  editForm.webhook_ready = row.webhook_ready
  editForm.interval_sec = row.interval_sec
  editVisible.value = true
}

async function saveEdit() {
  if (editForm.watches.length === 0) {
    ElMessage.warning('请至少选择一项关注内容')
    return
  }
  saving.value = true
  try {
    const body = { interval_sec: editForm.interval_sec, use_webhook: editForm.use_webhook }
    // 只提交发生变化的关注项，避免把未动的字段误关
    watchOptions.forEach((opt) => {
      body[opt.key] = editForm.watches.includes(opt.key)
    })
    if (editForm.token) body.token = editForm.token
    await api.put(`/api/v1/sources/${editForm.id}`, body)
    ElMessage.success('已保存')
    editVisible.value = false
    load()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    saving.value = false
  }
}

async function toggleSub(row, subscribe) {
  try {
    if (subscribe) {
      await api.post(`/api/v1/sources/${row.id}/subscription`)
      ElMessage.success(`已订阅 ${row.owner}/${row.repo}`)
    } else {
      await api.del(`/api/v1/sources/${row.id}/subscription`)
      ElMessage.success('已退订')
    }
    row.subscribed = subscribe
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function toggleEnabled(row, enabled) {
  try {
    await api.put(`/api/v1/sources/${row.id}`, { enabled })
    row.enabled = enabled
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function remove(row) {
  try {
    await ElMessageBox.confirm(
      `确定删除消息源「${row.owner}/${row.repo}」？将同步清理 GitHub webhook，已转存的消息保留。`,
      '删除消息源',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await api.del(`/api/v1/sources/${row.id}`)
    ElMessage.success('已删除')
    load()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

onMounted(() => {
  load()
  api
    .get('/api/v1/settings')
    .then((data) => (baseURLConfigured.value = !!data.base_url_configured))
    .catch(() => {})
})
</script>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.toolbar .el-alert {
  flex: 1;
}
.spacer {
  width: 10px;
}
.repo {
  display: flex;
  flex-direction: column;
}
.repo-name {
  font-weight: 500;
}
.repo-topic {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
.watch-tag {
  margin-right: 4px;
  margin-bottom: 2px;
}
.dim {
  color: var(--el-text-color-secondary);
}
.run-at {
  font-size: 12px;
}
.repo-input {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
}
.slash {
  color: var(--el-text-color-secondary);
}
.hint {
  color: var(--el-text-color-secondary);
  font-size: 12px;
  line-height: 1.6;
}
.hint.inline {
  margin-left: 10px;
}
.hint.warn {
  color: var(--el-color-warning);
  margin-top: 4px;
  width: 100%;
}
</style>

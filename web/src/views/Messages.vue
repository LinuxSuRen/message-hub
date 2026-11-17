<template>
  <div>
    <div class="toolbar">
      <el-radio-group v-model="scope" @change="reload">
        <el-radio-button value="all">全部</el-radio-button>
        <el-radio-button value="own">本租户</el-radio-button>
        <el-radio-button value="public">公开</el-radio-button>
      </el-radio-group>
      <el-select v-model="topicFilter" clearable placeholder="全部主题" class="filter-select" @change="reload">
        <el-option v-for="src in sources" :key="src.id" :label="`${src.owner}/${src.repo}`" :value="src.topic" />
      </el-select>
      <el-select v-model="levelFilter" clearable placeholder="全部级别" class="filter-select" @change="reload">
        <el-option label="信息" value="info" />
        <el-option label="警告" value="warning" />
        <el-option label="错误" value="error" />
      </el-select>
      <el-input
        v-model="keyword" placeholder="搜索标题 / 正文关键字" class="search-input" clearable
        @keyup.enter="reload" @clear="reload">
        <template #append>
          <el-button @click="reload">搜索</el-button>
        </template>
      </el-input>
      <div class="spacer" />
      <el-tag :type="live ? 'success' : 'info'" effect="dark" size="small">
        {{ live ? '实时连接中' : '连接断开，自动重连中' }}
      </el-tag>
      <el-button :loading="loading" @click="reload">刷新</el-button>
      <el-button type="primary" @click="openCompose">发布消息</el-button>
    </div>

    <el-table :data="items" v-loading="loading" @row-click="open" class="msg-table" empty-text="暂无消息">
      <el-table-column label="级别" width="90">
        <template #default="{ row }">
          <el-tag :type="levelTagType[row.level] || 'info'">{{ levelText[row.level] || row.level }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="title" label="标题" min-width="200" show-overflow-tooltip />
      <el-table-column prop="topic" label="主题" width="140" show-overflow-tooltip>
        <template #default="{ row }">{{ row.topic || '—' }}</template>
      </el-table-column>
      <el-table-column label="可见性" width="90">
        <template #default="{ row }">
          <el-tag v-if="row.public" type="success" size="small">公开</el-tag>
          <el-tag v-else type="info" size="small">租户</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="时间" width="170">
        <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
      </el-table-column>
    </el-table>

    <div class="pager">
      <el-pagination
        background layout="total, sizes, prev, pager, next, jumper"
        :total="total" :current-page="page" :page-size="pageSize"
        :page-sizes="[20, 50, 100]"
        @current-change="onPageChange" @size-change="onPageSizeChange" />
    </div>

    <el-dialog v-model="detail" :title="detail?.title" width="560px">
      <p class="detail-meta">
        <el-tag :type="levelTagType[detail?.level] || 'info'">{{ levelText[detail?.level] || detail?.level }}</el-tag>
        <span v-if="detail?.topic">主题：{{ detail.topic }}</span>
        <span>{{ fmtTime(detail?.created_at) }}</span>
      </p>
      <pre class="detail-body">{{ detail?.body || '（无正文）' }}</pre>
    </el-dialog>

    <el-dialog v-model="composeVisible" title="发布消息" width="520px">
      <el-form label-width="70px">
        <el-form-item label="标题" required>
          <el-input v-model="compose.title" maxlength="128" show-word-limit placeholder="消息标题" />
        </el-form-item>
        <el-form-item label="正文">
          <el-input v-model="compose.body" type="textarea" :rows="4" maxlength="4096" show-word-limit placeholder="消息正文（可选）" />
        </el-form-item>
        <el-form-item label="主题">
          <el-input v-model="compose.topic" maxlength="64" placeholder="可选，例如 ci、github/linuxsuren/message-hub" />
        </el-form-item>
        <el-form-item label="级别">
          <el-radio-group v-model="compose.level">
            <el-radio-button value="info">信息</el-radio-button>
            <el-radio-button value="warning">警告</el-radio-button>
            <el-radio-button value="error">错误</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item v-if="canPublic" label="可见范围">
          <el-switch v-model="compose.public" active-text="公开（全员可见）" inactive-text="本租户" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="composeVisible = false">取消</el-button>
        <el-button type="primary" :loading="composing" @click="publish">发布</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage, ElNotification } from 'element-plus'
import { api, fmtTime, levelText, levelTagType } from '../api'
import { subscribeMessages } from '../events'

const props = defineProps({ user: Object })

const items = ref([])
const scope = ref('all')
const topicFilter = ref('')
const levelFilter = ref('')
const keyword = ref('')
const sources = ref([])
const page = ref(1)
const pageSize = ref(50)
const total = ref(0)
const loading = ref(false)
const live = ref(false)
const detail = ref(null)
let unsubscribe = null

// 发布消息（登录会话即可；公开消息仅管理员角色可见开关）
const composeVisible = ref(false)
const composing = ref(false)
const compose = reactive({ title: '', body: '', topic: '', level: 'info', public: false })
const canPublic = computed(() => props.user && props.user.role !== 'user')

function openCompose() {
  compose.title = ''
  compose.body = ''
  compose.topic = ''
  compose.level = 'info'
  compose.public = false
  composeVisible.value = true
}

async function publish() {
  if (!compose.title.trim()) {
    ElMessage.warning('请填写标题')
    return
  }
  composing.value = true
  try {
    await api.post('/api/v1/messages', { ...compose })
    ElMessage.success('已发布')
    composeVisible.value = false
    reload()
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    composing.value = false
  }
}

// 查询与翻页：任何条件变化页码归 1（查询、筛选、每页条数）
async function reload() {
  page.value = 1
  await load()
}

async function load() {
  loading.value = true
  try {
    const query = new URLSearchParams({
      page: String(page.value),
      page_size: String(pageSize.value),
      scope: scope.value,
    })
    if (topicFilter.value) query.set('topic', topicFilter.value)
    if (levelFilter.value) query.set('level', levelFilter.value)
    if (keyword.value.trim()) query.set('q', keyword.value.trim())
    const data = await api.get(`/api/v1/messages?${query}`)
    items.value = data.items || []
    total.value = data.total ?? 0
  } catch (e) {
    ElNotification.error({ title: '消息加载失败', message: e.message })
  } finally {
    loading.value = false
  }
}

function onPageChange(p) {
  page.value = p
  load()
}

function onPageSizeChange(size) {
  pageSize.value = size
  if (page.value === 1) {
    load()
  } else {
    page.value = 1 // 触发 current-change 再加载，避免双重请求
  }
}

function open(row) {
  detail.value = row
}

onMounted(() => {
  load()
  api
    .get('/api/v1/sources')
    .then((data) => (sources.value = data.items || []))
    .catch(() => {})
  unsubscribe = subscribeMessages(
    (msg) => {
      if (items.value.some((it) => it.id === msg.id)) return
      // 翻页浏览时不打扰当前页；回第 1 页或刷新即可看到
      if (page.value !== 1) return
      if (scope.value === 'public' && !msg.public) return
      if (scope.value === 'own' && msg.public) return
      if (topicFilter.value && msg.topic !== topicFilter.value) return
      if (levelFilter.value && msg.level !== levelFilter.value) return
      const kw = keyword.value.trim()
      if (kw && !(`${msg.title} ${msg.body}`.includes(kw))) return
      items.value.unshift(msg)
      total.value += 1
      ElNotification({
        title: msg.title,
        message: msg.body || msg.title,
        type: msg.level === 'error' ? 'error' : msg.level === 'warning' ? 'warning' : 'info',
      })
    },
    (ok) => {
      live.value = ok
    },
  )
})

onUnmounted(() => {
  if (unsubscribe) unsubscribe()
})
</script>

<style scoped>
.toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
  flex-wrap: wrap;
}
.filter-select {
  width: 160px;
}
.search-input {
  width: 260px;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}
.spacer {
  flex: 1;
}
.msg-table {
  cursor: pointer;
}
.detail-meta {
  display: flex;
  gap: 12px;
  align-items: center;
  color: var(--el-text-color-secondary);
  font-size: 13px;
}
.detail-body {
  white-space: pre-wrap;
  word-break: break-all;
  background: #f5f7fa;
  padding: 12px;
  border-radius: 4px;
  font-family: inherit;
}
</style>

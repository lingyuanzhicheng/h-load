<script setup lang="ts">
import { LogoGithub } from "@vicons/ionicons5";

const projectUrl = "https://github.com/lingyuanzhicheng/h-load";
const authorUrl = "https://github.com/tbphp";
</script>

<template>
  <footer class="app-footer">
    <div class="footer-container">
      <!-- 主要信息区 -->
      <div class="footer-main">
        <a :href="projectUrl" target="_blank" rel="noopener noreferrer" class="version-container version-clickable">
          <n-icon :component="LogoGithub" color="#f0a020" :size="14" class="version-icon" />
          <span class="version-text">
            Github -
            <span class="version-status">(2026/7/1)</span>
          </span>
        </a>

        <!-- 版权信息 -->
        <div class="copyright-container">
          <span class="copyright-text">
            © 2026 Fork by
            <a :href="authorUrl" target="_blank" rel="noopener noreferrer" class="author-link">tbphp</a>
          </span>
          <span class="license-text">MIT License</span>
        </div>
      </div>
    </div>
  </footer>
</template>

<style scoped>
.app-footer {
  background: var(--footer-bg);
  backdrop-filter: blur(20px);
  border-top: 1px solid var(--border-color-light);
  padding: 12px 24px;
  font-size: 14px;
  min-height: 52px;
}

.footer-container {
  max-width: 1200px;
  margin: 0 auto;
}

.footer-main {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  line-height: 1.4;
}

.version-container {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 8px;
  border-radius: 6px;
  transition: all 0.2s ease;
  text-decoration: none;
}

.version-icon {
  display: flex;
  align-items: center;
}

.version-text {
  font-weight: 500;
  font-size: 13px;
  color: var(--text-secondary);
  white-space: nowrap;
}

.version-status {
  color: #f0a020;
}

.version-clickable {
  cursor: pointer;
}

.version-clickable:hover {
  background: rgba(240, 160, 32, 0.1);
  transform: translateY(-1px);
}

/* 版权信息区域 */
.copyright-container {
  display: flex;
  align-items: center;
  gap: 8px;
}

.copyright-text {
  color: var(--text-tertiary);
  font-size: 12px;
}

.license-text {
  color: var(--text-tertiary);
  font-size: 12px;
}

.author-link {
  font-weight: 600;
  color: var(--primary-color);
  text-decoration: none;
}

.author-link:hover {
  text-decoration: underline !important;
}

/* 响应式设计 */
@media (max-width: 768px) {
  .app-footer {
    padding: 10px 16px;
    height: auto;
  }

  .footer-main {
    flex-direction: column;
    gap: 8px;
    text-align: center;
  }

  .footer-main :deep(.n-divider) {
    display: none;
  }
}

@media (max-width: 480px) {
  .footer-main {
    gap: 6px;
  }

  .version-text {
    font-size: 12px;
  }
}
</style>

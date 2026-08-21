import { createApp } from 'vue'
import App from './App.vue'
import './style.css'
// 先于挂载求值，让 <html data-theme> 在首帧之前就位，避免主题闪烁。
import './theme'

createApp(App).mount('#app')

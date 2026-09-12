/// <reference types="vite/client" />

// Vue 单文件组件的模块声明。没有它 tsc 不认识 .vue 导入。
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<{}, {}, any>
  export default component
}

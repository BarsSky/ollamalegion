// OllamaLegion Monitor — runtime configuration
// Может быть переопределён сервером через встраивание <script>window.WEBUI_CONFIG = {...}</script>
(function() {
  'use strict';
  window.WEBUI_CONFIG = window.WEBUI_CONFIG || {
    apiBase: '',
    dashboardUrl: '/'
  };
})();
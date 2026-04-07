return {
  {
    "mason-org/mason.nvim",
    opts = {
      ensure_installed = {},
    },
  },
  {
    "neovim/nvim-lspconfig",
    opts = {
      servers = {
        lua_ls = {
          mason = false,
          cmd = { "lua-language-server" },
        },
        gopls = {
          mason = false,
          cmd = { "gopls" },
        },
        ols = {
          mason = false,
          cmd = { "ols" },
          init_options = {
            odin_command = "odin",
          },
        },
      },
    },
  },
}

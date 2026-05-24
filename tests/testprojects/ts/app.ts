import { Data } from './services/UserService';
const store = new Data.UserStore();
store.getUser('1');
